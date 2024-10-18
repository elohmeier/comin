package manager

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/builder"
	"github.com/nlewo/comin/internal/fetcher"
	"github.com/nlewo/comin/internal/prometheus"
	"github.com/nlewo/comin/internal/repository"
	"github.com/nlewo/comin/internal/scheduler"
	"github.com/nlewo/comin/internal/store"
	"github.com/nlewo/comin/internal/utils"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

type metricsMock struct{}

func (m metricsMock) SetDeploymentInfo(commitId, status string) {}

var mkNixEvalMock = func(evalOk chan bool) builder.EvalFunc {
	return func(ctx context.Context, repositoryPath string, hostname string) (string, string, string, error) {
		ok := <-evalOk
		if ok {
			return "drv-path", "out-path", "", nil
		} else {
			return "", "", "", fmt.Errorf("An error occured")
		}
	}
}

var mkNixBuildMock = func(buildOk chan bool) builder.BuildFunc {
	return func(ctx context.Context, drvPath string) error {
		ok := <-buildOk
		if ok {
			return nil
		} else {
			return fmt.Errorf("An error occured")
		}
	}
}

func TestBuild(t *testing.T) {
	evalOk := make(chan bool)
	buildOk := make(chan bool)
	logrus.SetLevel(logrus.DebugLevel)
	r := utils.NewRepositoryMock()
	f := fetcher.NewFetcher(r)
	f.Start()
	b := builder.New("my-machine", 2*time.Second, mkNixEvalMock(evalOk), 2*time.Second, mkNixBuildMock(buildOk))
	m := New(r, store.New("", 1, 1), prometheus.New(), scheduler.New(), f, b, "", "", "", "")
	go m.Run()
	assert.False(t, m.fetcher.State().IsFetching)
	assert.False(t, m.builder.State().IsEvaluating)
	assert.False(t, m.builder.State().IsBuilding)

	commitId := "id-1"
	f.TriggerFetch([]string{"remote"})
	r.RsCh <- repository.RepositoryStatus{
		SelectedCommitId: commitId,
	}
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.True(c, m.builder.State().IsEvaluating)
		assert.False(c, m.builder.State().IsBuilding)
	}, 5*time.Second, 100*time.Millisecond)

	// This simulates the failure of an evaluation
	evalOk <- false
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.False(c, m.builder.State().IsEvaluating)
		assert.False(c, m.builder.State().IsBuilding)
		assert.NotNil(c, m.builder.GetGeneration().EvalErr)
		assert.False(c, m.generationAvailable)
	}, 5*time.Second, 100*time.Millisecond)

	commitId = "id-2"
	f.TriggerFetch([]string{"remote"})
	r.RsCh <- repository.RepositoryStatus{
		SelectedCommitId: commitId,
	}
	// This simulates the success of an evaluation
	evalOk <- true
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.False(c, m.builder.State().IsEvaluating)
		assert.True(c, m.builder.State().IsBuilding)
		assert.Nil(c, m.builder.GetGeneration().EvalErr)
		assert.False(c, m.generationAvailable)
	}, 5*time.Second, 100*time.Millisecond)

	// This simulates the failure of a build
	buildOk <- false
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.False(c, m.builder.State().IsEvaluating)
		assert.False(c, m.builder.State().IsBuilding)
		assert.NotNil(c, m.builder.GetGeneration().BuildErr)
		assert.False(c, m.generationAvailable)
	}, 5*time.Second, 100*time.Millisecond)

	// This simulates the failure of a build
	f.TriggerFetch([]string{"remote"})
	r.RsCh <- repository.RepositoryStatus{
		SelectedCommitId: "id-3",
	}
	evalOk <- true
	buildOk <- true
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.False(c, m.builder.State().IsEvaluating)
		assert.False(c, m.builder.State().IsBuilding)
		assert.Nil(c, m.builder.GetGeneration().BuildErr)
		assert.True(c, m.generationAvailable)
		logrus.Infof("6 HEERE %v", (*m).generationAvailable)
	}, 5*time.Second, 100*time.Millisecond)
}

// func TestRun(t *testing.T) {
// 	logrus.SetLevel(logrus.DebugLevel)
// 	r := utils.NewRepositoryMock()
// 	f := fetcher.NewFetcher(r)
// 	f.Start()
// 	m := New(r, store.New("", 1, 1), prometheus.New(), scheduler.New(), f, "", "", "", "")

// 	evalDone := make(chan struct{})
// 	buildDone := make(chan struct{})
// 	nixEvalMock := func(ctx context.Context, repositoryPath string, hostname string) (string, string, string, error) {
// 		<-evalDone
// 		return "drv-path", "out-path", "", nil
// 	}
// 	nixBuildMock := func(ctx context.Context, drvPath string) error {
// 		<-buildDone
// 		return nil
// 	}
// 	m.evalFunc = nixEvalMock
// 	m.buildFunc = nixBuildMock

// 	deployFunc := func(context.Context, string, string, string) (bool, string, error) {
// 		return false, "", nil
// 	}
// 	m.deployerFunc = deployFunc

// 	go m.Run()

// 	// the state is empty
// 	assert.Equal(t, State{}, m.GetState())

// 	// the repository is fetched
// 	m.fetcher.TriggerFetch([]string{"origin"})
// 	assert.Equal(t, repository.RepositoryStatus{}, m.GetState().RepositoryStatus)

// 	// we inject a repositoryStatus
// 	r.RsCh <- repository.RepositoryStatus{
// 		SelectedCommitId: "foo",
// 	}
// 	assert.Equal(
// 		t,
// 		repository.RepositoryStatus{SelectedCommitId: "foo"},
// 		m.GetState().RepositoryStatus)

// 	// we simulate the end of the evaluation
// 	close(evalDone)
// 	assert.EventuallyWithT(t, func(c *assert.CollectT) {
// 		assert.Equal(c, "drv-path", m.GetState().Generation.DrvPath)
// 		assert.NotEmpty(c, m.GetState().Generation.EvalEndedAt)
// 	}, 5*time.Second, 100*time.Millisecond, "evaluation is not finished")

// 	// we simulate the end of the build
// 	close(buildDone)
// 	assert.EventuallyWithT(t, func(c *assert.CollectT) {
// 		assert.NotEmpty(c, m.GetState().Generation.BuildEndedAt)
// 	}, 5*time.Second, 100*time.Millisecond, "build is not finished")

// 	// we simulate the end of the deploy
// 	assert.EventuallyWithT(t, func(c *assert.CollectT) {
// 		assert.NotEmpty(c, m.GetState().Deployment.EndAt)
// 	}, 5*time.Second, 100*time.Millisecond, "deployment is not finished")

// }

// func TestFetchBusy(t *testing.T) {
// 	logrus.SetLevel(logrus.DebugLevel)
// 	r := utils.NewRepositoryMock()
// 	f := fetcher.NewFetcher(r)
// 	f.Start()
// 	m := New(r, store.New("", 1, 1), prometheus.New(), scheduler.New(), f, "", "", "", "machine-id")
// 	go m.Run()

// 	assert.Equal(t, State{}, m.GetState())

// 	m.fetcher.TriggerFetch([]string{"origin"})
// 	assert.Equal(t, repository.RepositoryStatus{}, m.GetState().RepositoryStatus)

// 	m.fetcher.TriggerFetch([]string{"origin"})
// 	assert.Equal(t, repository.RepositoryStatus{}, m.GetState().RepositoryStatus)
// }

// func TestRestartComin(t *testing.T) {
// 	logrus.SetLevel(logrus.DebugLevel)
// 	r := utils.NewRepositoryMock()
// 	f := fetcher.NewFetcher(r)
// 	f.Start()
// 	m := New(r, store.New("", 1, 1), prometheus.New(), scheduler.New(), f, "", "", "", "machine-id")
// 	dCh := make(chan deployment.DeploymentResult)
// 	m.deploymentResultCh = dCh
// 	isCominRestarted := false
// 	cominServiceRestartMock := func() error {
// 		isCominRestarted = true
// 		return nil
// 	}
// 	m.cominServiceRestartFunc = cominServiceRestartMock
// 	go m.Run()
// 	m.deploymentResultCh <- deployment.DeploymentResult{
// 		RestartComin: true,
// 	}
// 	assert.EventuallyWithT(t, func(c *assert.CollectT) {
// 		assert.True(c, isCominRestarted)
// 	}, 5*time.Second, 100*time.Millisecond, "comin has not been restarted yet")

// }

// func TestOptionnalMachineId(t *testing.T) {
// 	logrus.SetLevel(logrus.DebugLevel)
// 	r := utils.NewRepositoryMock()
// 	f := fetcher.NewFetcher(r)
// 	f.Start()
// 	m := New(r, store.New("", 1, 1), prometheus.New(), scheduler.New(), f, "", "", "", "the-test-machine-id")

// 	evalDone := make(chan struct{})
// 	buildDone := make(chan struct{})
// 	nixEvalMock := func(ctx context.Context, repositoryPath string, hostname string) (string, string, string, error) {
// 		<-evalDone
// 		// When comin.machineId is empty, comin evaluates it as an empty string
// 		evaluatedMachineId := ""
// 		return "drv-path", "out-path", evaluatedMachineId, nil
// 	}
// 	nixBuildMock := func(ctx context.Context, drvPath string) error {
// 		<-buildDone
// 		return nil
// 	}
// 	m.evalFunc = nixEvalMock
// 	m.buildFunc = nixBuildMock

// 	go m.Run()
// 	m.fetcher.TriggerFetch([]string{"origin"})
// 	r.RsCh <- repository.RepositoryStatus{SelectedCommitId: "foo"}

// 	// we simulate the end of the evaluation
// 	close(evalDone)

// 	assert.EventuallyWithT(t, func(c *assert.CollectT) {
// 		assert.True(t, m.GetState().IsRunning)
// 	}, 5*time.Second, 100*time.Millisecond, "evaluation is not finished")
// }

// func TestIncorrectMachineId(t *testing.T) {
// 	logrus.SetLevel(logrus.DebugLevel)
// 	r := utils.NewRepositoryMock()
// 	f := fetcher.NewFetcher(r)
// 	f.Start()
// 	m := New(r, store.New("", 1, 1), prometheus.New(), scheduler.New(), f, "", "", "", "the-test-machine-id")

// 	evalDone := make(chan struct{})
// 	buildDone := make(chan struct{})
// 	nixEvalMock := func(ctx context.Context, repositoryPath string, hostname string) (string, string, string, error) {
// 		<-evalDone
// 		return "drv-path", "out-path", "incorrect-machine-id", nil
// 	}
// 	nixBuildMock := func(ctx context.Context, drvPath string) error {
// 		<-buildDone
// 		return nil
// 	}
// 	m.evalFunc = nixEvalMock
// 	m.buildFunc = nixBuildMock

// 	go m.Run()

// 	// the state is empty
// 	assert.Equal(t, State{}, m.GetState())

// 	// the repository is fetched
// 	m.fetcher.TriggerFetch([]string{"origin"})
// 	r.RsCh <- repository.RepositoryStatus{SelectedCommitId: "foo"}

// 	assert.True(t, m.GetState().IsRunning)

// 	// we simulate the end of the evaluation
// 	close(evalDone)

// 	assert.EventuallyWithT(t, func(c *assert.CollectT) {
// 		// The manager is no longer running since the machine id are not identical
// 		assert.False(t, m.GetState().IsRunning)
// 	}, 5*time.Second, 100*time.Millisecond, "evaluation is not finished")
// }
