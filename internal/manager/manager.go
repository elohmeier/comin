package manager

import (
	"context"
	"fmt"

	"github.com/nlewo/comin/internal/deployment"
	"github.com/nlewo/comin/internal/generation"
	"github.com/nlewo/comin/internal/nix"
	"github.com/nlewo/comin/internal/prometheus"
	"github.com/nlewo/comin/internal/repository"
	"github.com/nlewo/comin/internal/storage"
	"github.com/nlewo/comin/internal/utils"
	"github.com/sirupsen/logrus"
)

type State struct {
	RepositoryStatus repository.RepositoryStatus `json:"repository_status"`
	Generation       generation.Generation
	IsFetching       bool                  `json:"is_fetching"`
	IsRunning        bool                  `json:"is_running"`
	Deployment       deployment.Deployment `json:"deployment"`
	Hostname         string                `json:"hostname"`
}

type Manager struct {
	repository repository.Repository
	// FIXME: a generation should get a repository URL from the repository status
	repositoryPath string
	hostname       string
	// The machine id of the current host
	machineId         string
	triggerRepository chan string
	generationFactory func(repository.RepositoryStatus, string, string) generation.Generation
	stateRequestCh    chan struct{}
	stateResultCh     chan State
	repositoryStatus  repository.RepositoryStatus
	// The generation currently managed
	generation generation.Generation
	isFetching bool
	// FIXME: this is temporary in order to simplify the manager
	// for a first iteration: this needs to be removed
	isRunning               bool
	needToBeRestarted       bool
	cominServiceRestartFunc func() error

	evalFunc  generation.EvalFunc
	buildFunc generation.BuildFunc

	deploymentResultCh chan deployment.DeploymentResult
	// The deployment currenly managed
	deployment   deployment.Deployment
	deployerFunc deployment.DeployFunc

	repositoryStatusCh  chan repository.RepositoryStatus
	triggerDeploymentCh chan generation.Generation

	prometheus prometheus.Prometheus
	storage    storage.Storage

	requestDeploymentListCh  chan struct{}
	responseDeploymentListCh chan []deployment.Deployment
}

func New(r repository.Repository, p prometheus.Prometheus, s storage.Storage, path, hostname, machineId string) Manager {
	return Manager{
		repository:               r,
		repositoryPath:           path,
		hostname:                 hostname,
		machineId:                machineId,
		evalFunc:                 nix.Eval,
		buildFunc:                nix.Build,
		deployerFunc:             nix.Deploy,
		triggerRepository:        make(chan string),
		stateRequestCh:           make(chan struct{}),
		stateResultCh:            make(chan State),
		cominServiceRestartFunc:  utils.CominServiceRestart,
		deploymentResultCh:       make(chan deployment.DeploymentResult),
		repositoryStatusCh:       make(chan repository.RepositoryStatus),
		prometheus:               p,
		storage:                  s,
		triggerDeploymentCh:      make(chan generation.Generation, 1),
		requestDeploymentListCh:  make(chan struct{}),
		responseDeploymentListCh: make(chan []deployment.Deployment),
	}
}

func (m Manager) GetState() State {
	m.stateRequestCh <- struct{}{}
	return <-m.stateResultCh
}

func (m Manager) GetDeploymentList() []deployment.Deployment {
	m.requestDeploymentListCh <- struct{}{}
	return <-m.responseDeploymentListCh
}

func (m Manager) Fetch(remote string) {
	m.triggerRepository <- remote
}

func (m Manager) toState() State {
	return State{
		Generation:       m.generation,
		RepositoryStatus: m.repositoryStatus,
		IsFetching:       m.isFetching,
		IsRunning:        m.isRunning,
		Deployment:       m.deployment,
		Hostname:         m.hostname,
	}
}

func (m Manager) onEvaluated(ctx context.Context, evalResult generation.EvalResult) Manager {
	m.generation = m.generation.UpdateEval(evalResult)
	if evalResult.Err == nil {
		m.generation = m.generation.Build(ctx)
	} else {
		m.isRunning = false
	}
	return m
}

func (m Manager) onBuilt(ctx context.Context, buildResult generation.BuildResult) Manager {
	m.generation = m.generation.UpdateBuild(buildResult)
	if buildResult.Err == nil {
		m.triggerDeployment(ctx, m.generation)
	} else {
		m.isRunning = false
	}
	return m
}

func (m Manager) triggerDeployment(ctx context.Context, g generation.Generation) {
	m.triggerDeploymentCh <- g
}

func (m Manager) onTriggerDeployment(ctx context.Context, g generation.Generation) Manager {
	m.deployment = deployment.New(g, m.deployerFunc, m.deploymentResultCh)
	m.deployment = m.deployment.Deploy(ctx)
	return m
}

func (m Manager) onDeployment(ctx context.Context, deploymentResult deployment.DeploymentResult) Manager {
	logrus.Debugf("Deploy done with %#v", deploymentResult)
	m.deployment = m.deployment.Update(deploymentResult)
	// The comin service is not restart by the switch-to-configuration script in order to let comin terminating properly. Instead, comin restarts itself.
	if m.deployment.RestartComin {
		m.needToBeRestarted = true
	}
	m.isRunning = false
	m.prometheus.SetDeploymentInfo(m.deployment.Generation.SelectedCommitId, deployment.StatusToString(m.deployment.Status))
	err := m.storage.DeploymentInsert(m.deployment)
	if err != nil {
		logrus.Errorf("Failed to store the deployment: %s", err)
	}
	return m
}

func (m Manager) onRepositoryStatus(ctx context.Context, rs repository.RepositoryStatus) Manager {
	logrus.Debugf("Fetch done with %#v", rs)
	m.isFetching = false
	m.repositoryStatus = rs

	for _, r := range rs.Remotes {
		if r.LastFetched {
			status := "failed"
			if r.FetchErrorMsg == "" {
				status = "succeeded"
			}
			m.prometheus.IncFetchCounter(r.Name, status)
		}
	}

	if rs.SelectedCommitId == m.generation.SelectedCommitId && rs.SelectedBranchIsTesting == m.generation.SelectedBranchIsTesting {
		logrus.Debugf("The repository status is the same than the previous one")
		m.isRunning = false
	} else {
		// g.Stop(): this is required once we remove m.IsRunning
		flakeUrl := fmt.Sprintf("git+file://%s?rev=%s", m.repositoryPath, m.repositoryStatus.SelectedCommitId)
		m.generation = generation.New(rs, flakeUrl, m.hostname, m.machineId, m.evalFunc, m.buildFunc)
		m.generation = m.generation.Eval(ctx)
	}
	return m
}

func (m Manager) onTriggerRepository(ctx context.Context, remoteName string) Manager {
	if m.isFetching {
		logrus.Debugf("The manager is already fetching the repository")
		return m
	}
	// FIXME: we will remove this in future versions
	if m.isRunning {
		logrus.Debugf("The manager is already running: it is currently not able to run tasks in parallel")
		return m
	}
	logrus.Debugf("Trigger fetch and update remote %s", remoteName)
	m.isRunning = true
	m.isFetching = true
	m.repositoryStatusCh = m.repository.FetchAndUpdate(ctx, remoteName)
	return m
}

func (m Manager) onRequestDeploymentList(ctx context.Context) {
	d, err := m.storage.DeploymentList(ctx, 10)
	if err != nil {
		logrus.Errorf("Failed to get the deployment list: %s", err)
	}
	m.responseDeploymentListCh <- d
}

func (m Manager) Run() {
	ctx := context.TODO()

	logrus.Info("The manager is started")
	logrus.Infof("  hostname = %s", m.hostname)
	logrus.Infof("  machineId = %s", m.machineId)
	logrus.Infof("  repositoryPath = %s", m.repositoryPath)
	for {
		select {
		case <-m.stateRequestCh:
			m.stateResultCh <- m.toState()
		case <-m.requestDeploymentListCh:
			m.onRequestDeploymentList(ctx)
		case remoteName := <-m.triggerRepository:
			m = m.onTriggerRepository(ctx, remoteName)
		case rs := <-m.repositoryStatusCh:
			m = m.onRepositoryStatus(ctx, rs)
		case evalResult := <-m.generation.EvalCh():
			m = m.onEvaluated(ctx, evalResult)
		case buildResult := <-m.generation.BuildCh():
			m = m.onBuilt(ctx, buildResult)
		case generation := <-m.triggerDeploymentCh:
			m = m.onTriggerDeployment(ctx, generation)
		case deploymentResult := <-m.deploymentResultCh:
			m = m.onDeployment(ctx, deploymentResult)
		}
		if m.needToBeRestarted {
			// TODO: stop contexts
			if err := m.cominServiceRestartFunc(); err != nil {
				logrus.Fatal(err)
				return
			}

		}
	}
}
