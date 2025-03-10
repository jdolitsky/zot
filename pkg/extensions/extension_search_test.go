//go:build search
// +build search

package extensions_test

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	. "github.com/smartystreets/goconvey/convey"

	"zotregistry.io/zot/pkg/api/config"
	. "zotregistry.io/zot/pkg/extensions"
	cveinfo "zotregistry.io/zot/pkg/extensions/search/cve"
	"zotregistry.io/zot/pkg/log"
	"zotregistry.io/zot/pkg/meta/repodb"
	"zotregistry.io/zot/pkg/scheduler"
	"zotregistry.io/zot/pkg/storage"
	. "zotregistry.io/zot/pkg/test"
	"zotregistry.io/zot/pkg/test/mocks"
)

func TestTrivyDBGenerator(t *testing.T) {
	Convey("Test trivy task scheduler reset", t, func() {
		logFile, err := os.CreateTemp(t.TempDir(), "zot-log*.txt")
		logPath := logFile.Name()
		So(err, ShouldBeNil)

		defer os.Remove(logFile.Name()) // clean up

		logger := log.NewLogger("debug", logPath)
		writers := io.MultiWriter(os.Stdout, logFile)
		logger.Logger = logger.Output(writers)

		cfg := config.New()
		cfg.Scheduler = &config.SchedulerConfig{NumWorkers: 3}
		sch := scheduler.NewScheduler(cfg, logger)

		repoDB := &mocks.RepoDBMock{
			GetRepoMetaFn: func(repo string) (repodb.RepoMetadata, error) {
				return repodb.RepoMetadata{
					Tags: map[string]repodb.Descriptor{
						"tag": {MediaType: ispec.MediaTypeImageIndex},
					},
				}, nil
			},
		}
		storeController := storage.StoreController{
			DefaultStore: mocks.MockedImageStore{
				RootDirFn: func() string {
					return t.TempDir()
				},
			},
		}

		cveInfo := cveinfo.NewCVEInfo(storeController, repoDB, "ghcr.io/project-zot/trivy-db", "", logger)
		generator := NewTrivyTaskGenerator(time.Minute, cveInfo, logger)

		sch.SubmitGenerator(generator, 12000*time.Millisecond, scheduler.HighPriority)

		ctx, cancel := context.WithCancel(context.Background())

		sch.RunScheduler(ctx)

		defer cancel()

		// Wait for trivy db to download
		found, err := ReadLogFileAndCountStringOccurence(logPath,
			"DB update completed, next update scheduled", 120*time.Second, 2)
		So(err, ShouldBeNil)
		So(found, ShouldBeTrue)
	})
}
