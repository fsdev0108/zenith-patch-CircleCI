package cmd

import (
	"bytes"
	"fmt"
	"github.com/CircleCI-Public/circleci-cli/api/graphql"
	"github.com/CircleCI-Public/circleci-cli/api/rest"
	v1 "github.com/CircleCI-Public/circleci-cli/api/v1"
	"github.com/CircleCI-Public/circleci-cli/settings"
	"github.com/davecgh/go-spew/spew"
	"github.com/spf13/cobra"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type circleAPI struct {
	rc *rest.Client
}

func newCircleAPI(rc *rest.Client) *circleAPI {
	return &circleAPI{rc: rc}
}

type Project interface{}

func (a *circleAPI) GetProject(projectSlug string) (rc *Project, err error) {
	req, err := a.rc.NewRequest("GET", &url.URL{Path: "project/" + projectSlug}, nil)
	if err != nil {
		return nil, err
	}

	_, err = a.rc.DoRequest(req, rc)
	return rc, err
}

type zenithOptions struct {
	cfg     *settings.Config
	graphQL *graphql.Client
	rc      *rest.Client
	api     *circleAPI
	apiv1   *v1.Client
	args    []string

	account string
	repo    string

	filterBranch string
	filterStatus string
	buildLimit   int
	buildOffset  int
	download     bool

	// if true, only other-tests-{debug,release} builds will be used
	filterTests  bool
	filterFailed string // ""|"only"|"other"
}

func newZenithCommand(config *settings.Config) *cobra.Command {
	opts := zenithOptions{
		cfg:     config,
		account: "zenithdb",
		repo:    "zenith",
	}

	queryCommand := &cobra.Command{
		Use:    "zenith <magic>",
		Short:  "Query the CircleCI API to provide useful zenith info.",
		Hidden: true,
		PreRunE: func(_ *cobra.Command, args []string) error {
			opts.args = args
			opts.graphQL = graphql.NewClient(config.HTTPClient, config.Host, config.Endpoint, config.Token, config.Debug)
			opts.rc = rest.New(config.Host, config.RestEndpoint, config.Token)
			opts.api = newCircleAPI(opts.rc)
			opts.apiv1 = &v1.Client{Token: config.Token}

			return validateToken(opts.cfg)
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			return zenith(opts)
		},
		Args:        cobra.MinimumNArgs(1),
		Annotations: make(map[string]string),
	}
	queryCommand.Annotations["<magic>"] = "Some magic (TODO)"
	queryCommand.Flags().StringVarP(&opts.filterBranch, "branch", "b", "", "branch to filter builds")
	queryCommand.Flags().StringVarP(&opts.filterStatus, "status", "s", "", "status to filter builds")
	queryCommand.Flags().IntVarP(&opts.buildLimit, "fetch-limit", "", 100, "limit builds to fetch")
	queryCommand.Flags().IntVarP(&opts.buildOffset, "fetch-offset", "", 0, "offset builds to fetch")
	queryCommand.Flags().BoolVarP(&opts.filterTests, "filter-tests", "", true, "if true, only other-tests-{debug,release} builds will be used")
	queryCommand.Flags().StringVarP(&opts.filterFailed, "filter-failed", "", "only", "only = include only failed builds, other = only successful, \"\" = no filter")
	queryCommand.Flags().BoolVarP(&opts.download, "download", "", false, "if true, all artifacts for failed tests will be downloaded")

	return queryCommand
}

func fancyFilter(filter string) (bool, bool, bool) {
	only := filter == "only"
	other := filter == "other"
	return only, other, only || other
}

func zenith(opts zenithOptions) error {
	switch opts.args[0] {
	case "set-logs-dir":
		cfg := opts.cfg
		cfg.LogsDir = opts.args[1]
		spew.Dump(cfg)
		return cfg.WriteToDisk()
	default:
		// nothing to do
	}

	//q := opts.args[0]

	resp, err := opts.apiv1.ListRecentBuildsForProject(opts.account, opts.repo, opts.filterBranch, "", opts.buildLimit, opts.buildOffset)
	if err != nil {
		return err
	}

	const noPipeline = -1

	var builds []*buildWithInfo
	for _, b := range resp {
		if opts.filterTests {
			val := b.BuildParameters["CIRCLE_JOB"]
			if val != "other-tests-debug" && val != "other-tests-release" {
				continue
			}
		}

		if fail, _, ok := fancyFilter(opts.filterFailed); ok {
			if b.Failed == nil || fail != *b.Failed {
				continue
			}
		}

		buildInfo, err := fetchBuildInfo(opts, b)
		if err != nil {
			return err
		}

		builds = append(builds, buildInfo)
	}

	for _, b := range builds {
		fmt.Printf("======== BUILD %v, from %v, on branch %v ========\n", b.Build.BuildNum, b.Build.AuthorName, b.Build.Branch)
		for _, t := range b.FailedTests {
			info := b.testInfo(t)
			fmt.Printf(" -failed test %v\n", t.Name)
			for _, a := range info.Artifacts {
				a.LocalPath = filepath.Join(opts.cfg.LogsDir, b.PrettyName, t.Name, a.UniqueName)

				fmt.Printf("  *test artifact, name=%v", a.UniqueName)
				fmt.Printf(", local=%v", a.LocalPath)

				if opts.download {
					dir := filepath.Dir(a.LocalPath)
					err := os.MkdirAll(dir, 0777)
					if err != nil {
						return err
					}

					_, err = os.Stat(a.LocalPath)
					if os.IsNotExist(err) {
						err = a.download()
						if err != nil {
							return err
						}
					}
				}

				fmt.Println()
			}
		}
	}

	return nil
}

type buildWithInfo struct {
	Build       *v1.Build
	TestsMeta   []*v1.TestMetadata
	FailedTests []*v1.TestMetadata
	Artifacts   []*v1.Artifact
	PrettyName  string
}

func fetchBuildInfo(opts zenithOptions, b *v1.Build) (*buildWithInfo, error) {
	meta, err := opts.apiv1.ListTestMetadata(opts.account, opts.repo, b.BuildNum)
	if err != nil {
		return nil, err
	}

	var failedTests []*v1.TestMetadata
	for _, t := range meta {
		if t.Result == "failure" {
			failedTests = append(failedTests, t)
		}
	}

	artifacts, err := opts.apiv1.ListBuildArtifacts(opts.account, opts.repo, b.BuildNum)
	if err != nil {
		return nil, err
	}

	return &buildWithInfo{
		Build:       b,
		TestsMeta:   meta,
		FailedTests: failedTests,
		Artifacts:   artifacts,
		PrettyName:  strings.ReplaceAll(fmt.Sprintf("%v_%s", b.BuildNum, b.Branch), " ", "_"),
	}, nil
}

type testInfo struct {
	Meta      *v1.TestMetadata
	Artifacts []*testArtifact
}

func (i *buildWithInfo) testInfo(meta *v1.TestMetadata) *testInfo {
	var artifacts []*testArtifact
	for _, a := range i.Artifacts {
		if strings.Contains(a.Path, meta.Name) {
			artifacts = append(artifacts, &testArtifact{
				Artifact:   a,
				UniqueName: nameForArtifact(a.Path),
			})
		}
	}

	return &testInfo{
		Meta:      meta,
		Artifacts: artifacts,
	}
}

type testArtifact struct {
	Artifact   *v1.Artifact
	UniqueName string
	LocalPath  string
}

func (a *testArtifact) download() error {
	fmt.Printf(", [DOWNLOADING]")
	resp, err := http.Get(a.Artifact.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// TODO: download to tmp directory instead of memory
	buf := &bytes.Buffer{}
	written, err := io.Copy(buf, resp.Body)
	if err != nil {
		return err
	}

	fmt.Printf(", got %vkb", written/1024)
	return os.WriteFile(a.LocalPath, buf.Bytes(), 0777)
}

func nameForArtifact(path string) string {
	file := filepath.Base(path)
	dir := filepath.Base(filepath.Dir(path))
	if file == "safekeeper.log" || file == "wal_acceptor.log" {
		return dir + ".log"
	}
	return file
}
