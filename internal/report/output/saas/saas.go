package saas

import (
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/rs/zerolog/log"
	"golang.org/x/exp/maps"

	"github.com/bearer/bearer/api"
	"github.com/bearer/bearer/api/s3"
	"github.com/bearer/bearer/cmd/bearer/build"
	"github.com/bearer/bearer/internal/commands/process/gitrepository"
	"github.com/bearer/bearer/internal/commands/process/settings"
	saas "github.com/bearer/bearer/internal/report/output/saas/types"
	securitytypes "github.com/bearer/bearer/internal/report/output/security/types"
	"github.com/bearer/bearer/internal/report/output/types"
	"github.com/bearer/bearer/internal/util/file"
	util "github.com/bearer/bearer/internal/util/output"
	pointer "github.com/bearer/bearer/internal/util/pointers"
)

func GetReport(
	reportData *types.ReportData,
	config settings.Config,
	gitContext *gitrepository.Context,
	ensureMeta bool,
) error {
	var meta *saas.Meta
	meta, err := getMeta(reportData, config, gitContext)
	if err != nil {
		if ensureMeta {
			return err
		} else {
			meta = &saas.Meta{
				Target:         config.Scan.Target,
				FoundLanguages: reportData.FoundLanguages,
			}
		}
	}

	saasFindingsBySeverity := translateFindingsBySeverity(reportData.FindingsBySeverity)
	saasIgnoredFindingsBySeverity := translateFindingsBySeverity(reportData.IgnoredFindingsBySeverity)

	reportData.SaasReport = &saas.BearerReport{
		Meta:            *meta,
		Findings:        saasFindingsBySeverity,
		IgnoredFindings: saasIgnoredFindingsBySeverity,
		DataTypes:       reportData.Dataflow.Datatypes,
		Components:      reportData.Dataflow.Components,
		Errors:          reportData.Dataflow.Errors,
		Files:           getDiscoveredFiles(config, reportData.Files),
	}

	return nil
}

func SendReport(config settings.Config, reportData *types.ReportData, gitContext *gitrepository.Context) {
	if reportData.SaasReport == nil {
		err := GetReport(reportData, config, gitContext, true)
		if err != nil {
			errorMessage := fmt.Sprintf("Unable to calculate Metadata. %s", err)
			log.Debug().Msgf(errorMessage)
			config.Client.Error = &errorMessage
			return
		}
	}

	tmpDir, filename, err := createBearerGzipFileReport(config, reportData)
	if err != nil {
		config.Client.Error = pointer.String("Could not compress report.")
		log.Debug().Msgf("error creating report %s", err)
	}

	defer os.RemoveAll(*tmpDir)

	err = sendReportToBearer(config.Client, &reportData.SaasReport.Meta, filename)
	if err != nil {
		config.Client.Error = pointer.String("Report upload failed.")
		log.Debug().Msgf("error sending report to Bearer cloud: %s", err)
	}
}

func translateFindingsBySeverity[F securitytypes.GenericFinding](someFindingsBySeverity map[string][]F) map[string][]saas.SaasFinding {
	saasFindingsBySeverity := make(map[string][]saas.SaasFinding)
	for _, severity := range maps.Keys(someFindingsBySeverity) {
		for _, someFinding := range someFindingsBySeverity[severity] {
			finding := someFinding.GetFinding()
			saasFindingsBySeverity[severity] = append(saasFindingsBySeverity[severity], saas.SaasFinding{
				Finding:      finding,
				SeverityMeta: finding.SeverityMeta,
				IgnoreMeta:   someFinding.GetIgnoreMeta(),
			})
		}
	}
	return saasFindingsBySeverity
}

func sendReportToBearer(client *api.API, meta *saas.Meta, filename *string) error {
	fileUploadOffer, err := s3.UploadS3(&s3.UploadRequestS3{
		Api:             client,
		FilePath:        *filename,
		FilePrefix:      "bearer_security_report",
		ContentType:     "application/json",
		ContentEncoding: "gzip",
	})
	if err != nil {
		return err
	}

	meta.SignedID = fileUploadOffer.SignedID

	err = client.ScanFinished(meta)
	if err != nil {
		return err
	}

	return nil
}

func getDiscoveredFiles(config settings.Config, files []string) []string {
	filenames := make([]string, len(files))

	for i, filename := range files {
		filenames[i] = file.GetFullFilename(config.Scan.Target, filename)
	}

	return filenames
}

func createBearerGzipFileReport(
	config settings.Config,
	reportData *types.ReportData,
) (*string, *string, error) {
	tempDir, err := os.MkdirTemp("", "reports")
	if err != nil {
		return nil, nil, err
	}

	file, err := os.CreateTemp(tempDir, "security-*.json.gz")
	if err != nil {
		return &tempDir, nil, err
	}

	content, _ := util.ReportJSON(reportData.SaasReport)
	gzWriter := gzip.NewWriter(file)
	_, err = gzWriter.Write([]byte(content))
	if err != nil {
		return nil, nil, err
	}
	gzWriter.Close()

	filename := file.Name()

	return &tempDir, &filename, nil
}

func getMeta(
	reportData *types.ReportData,
	config settings.Config,
	gitContext *gitrepository.Context,
) (*saas.Meta, error) {
	if gitContext == nil {
		return nil, errors.New("not a git repository")
	}

	var messages []string
	if gitContext.Branch == "" {
		messages = append(messages,
			"Couldn't determine the name of the branch being scanned. "+
				"Please set the 'BEARER_BRANCH' environment variable.",
		)
	}
	if gitContext.DefaultBranch == "" {
		messages = append(messages,
			"Couldn't determine the default branch of the repository. "+
				"Please set the 'BEARER_DEFAULT_BRANCH' environment variable.",
		)
	}
	if gitContext.CommitHash == "" {
		messages = append(messages,
			"Couldn't determine the hash of the current commit of the repository. "+
				"Please set the 'BEARER_COMMIT' environment variable.",
		)
	}
	if gitContext.OriginURL == "" {
		messages = append(messages,
			"Couldn't determine the origin URL of the repository. "+
				"Please set the 'BEARER_REPOSITORY_URL' environment variable.",
		)
	}

	if len(messages) != 0 {
		return nil, errors.New(strings.Join(messages, "\n"))
	}

	return &saas.Meta{
		ID:                 gitContext.ID,
		Host:               gitContext.Host,
		Username:           gitContext.Owner,
		Name:               gitContext.Name,
		FullName:           gitContext.FullName,
		URL:                gitContext.OriginURL,
		Target:             config.Scan.Target,
		SHA:                gitContext.CommitHash,
		CurrentBranch:      gitContext.Branch,
		DefaultBranch:      gitContext.DefaultBranch,
		DiffBaseBranch:     gitContext.BaseBranch,
		BearerRulesVersion: config.BearerRulesVersion,
		BearerVersion:      build.Version,
		FoundLanguages:     reportData.FoundLanguages,
	}, nil
}
