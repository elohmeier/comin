package manager

import (
	"context"
	"fmt"
	"sync"

	"github.com/nlewo/comin/internal/builder"
	"github.com/nlewo/comin/internal/deployment"
	"github.com/nlewo/comin/internal/fetcher"
	"github.com/nlewo/comin/internal/generation"
	"github.com/nlewo/comin/internal/nix"
	"github.com/nlewo/comin/internal/profile"
	"github.com/nlewo/comin/internal/prometheus"
	"github.com/nlewo/comin/internal/repository"
	"github.com/nlewo/comin/internal/scheduler"
	"github.com/nlewo/comin/internal/store"
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
	NeedToReboot     bool                  `json:"need_to_reboot"`
}

type Manager struct {
	repository    repository.Repository
	repositoryDir string
	// FIXME: a generation should get a repository URL from the repository status
	repositoryPath string
	hostname       string
	// The machine id of the current host
	machineId         string
	generationFactory func(repository.RepositoryStatus, string, string) generation.Generation
	stateRequestCh    chan struct{}
	stateResultCh     chan State
	repositoryStatus  repository.RepositoryStatus
	// The generation currently managed
	generation generation.Generation
	// FIXME: this is temporary in order to simplify the manager
	// for a first iteration: this needs to be removed
	isRunning               bool
	needToBeRestarted       bool
	needToReboot            bool
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
	storage    store.Store
	scheduler  scheduler.Scheduler
	fetcher    *fetcher.Fetcher

	builder *builder.Builder

	buildMu               sync.Mutex
	generationToDeploy    builder.Generation
	generationAvailableCh chan struct{}
	generationAvailable   bool

	stop chan struct{}
}

func New(r repository.Repository, s store.Store, p prometheus.Prometheus, sched scheduler.Scheduler, fetcher *fetcher.Fetcher, builder *builder.Builder, path, dir, hostname, machineId string) *Manager {
	generationAvailable := false
	m := &Manager{
		repository:              r,
		repositoryDir:           dir,
		repositoryPath:          path,
		hostname:                hostname,
		machineId:               machineId,
		evalFunc:                nix.Eval,
		buildFunc:               nix.Build,
		deployerFunc:            nix.Deploy,
		stateRequestCh:          make(chan struct{}),
		stateResultCh:           make(chan State),
		cominServiceRestartFunc: utils.CominServiceRestart,
		deploymentResultCh:      make(chan deployment.DeploymentResult),
		repositoryStatusCh:      make(chan repository.RepositoryStatus),
		triggerDeploymentCh:     make(chan generation.Generation, 1),
		prometheus:              p,
		storage:                 s,
		scheduler:               sched,
		fetcher:                 fetcher,
		builder:                 builder,
		generationAvailableCh:   make(chan struct{}, 1),
		generationAvailable:     generationAvailable,
	}
	if len(s.DeploymentList()) > 0 {
		d := s.DeploymentList()[0]
		logrus.Infof("Restoring the manager state from the last deployment %s", d.UUID)
		m.deployment = d
		// TODO
		// m.generation = d.Generation
	}
	return m
}

func (m *Manager) GetState() State {
	m.stateRequestCh <- struct{}{}
	return <-m.stateResultCh
}

func (m *Manager) toState() State {
	return State{
		Generation:       m.generation,
		RepositoryStatus: m.repositoryStatus,
		IsFetching:       m.fetcher.IsFetching,
		IsRunning:        m.isRunning,
		Deployment:       m.deployment,
		Hostname:         m.hostname,
		NeedToReboot:     m.needToReboot,
	}
}

func (m *Manager) onRepositoryStatus(ctx context.Context, rs repository.RepositoryStatus) *Manager {
	logrus.Debugf("Fetch done with %#v", rs)
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

	if rs.SelectedCommitId == "" {
		logrus.Debugf("No commit has been selected from remotes")
		m.isRunning = false
	} else if rs.SelectedCommitId == m.generation.SelectedCommitId && rs.SelectedBranchIsTesting == m.generation.SelectedBranchIsTesting {
		logrus.Debugf("The repository status is the same than the previous one")
		m.isRunning = false
	} else {
		// g.Stop(): this is required once we remove m.IsRunning
		flakeUrl := fmt.Sprintf("git+file://%s?dir=%s&rev=%s", m.repositoryPath, m.repositoryDir, m.repositoryStatus.SelectedCommitId)
		m.generation = generation.New(rs, flakeUrl, m.hostname, m.machineId, m.evalFunc, m.buildFunc)
		m.generation = m.generation.Eval(ctx)
	}
	return m
}

// FetchAndBuild fetches new commits. If a new commit is available, it
// evaluates and builds the derivation. Once built, it pushes the
// generation on a channel which is consumed by the deployer.
func (m *Manager) FetchAndBuild() {
	go func() {
		for {
			select {
			case rs := <-m.fetcher.RepositoryStatusCh:
				flakeUrl := fmt.Sprintf("git+file://%s?dir=%s&rev=%s", m.repositoryPath, m.repositoryDir, rs.SelectedCommitId)
				m.builder.Eval(flakeUrl)
			case generation := <-m.builder.EvaluationDone:
				if generation.EvalErr == nil {
					logrus.Infof("manager: a generation is building: %v", generation)
					m.builder.Build()
				}
			case generation := <-m.builder.BuildDone:
				if generation.BuildErr == nil {
					logrus.Infof("manager: a generation is available for deployment: %v", generation)
					m.buildMu.Lock()
					m.generationToDeploy = generation
					if !m.generationAvailable {
						m.generationAvailable = true
						logrus.Infof("3 HEERE %v", m.generationAvailable)
						m.generationAvailableCh <- struct{}{}
						logrus.Infof("4 HEERE")
					}
					m.buildMu.Unlock()
					logrus.Infof("5 HEERE %v", m.generationAvailable)
				}
			}
		}

	}()
}

func (m *Manager) Deploy() {
	go func() {
		for {
			logrus.Debug("manager: waiting for a generation to deploy")
			<-m.generationAvailableCh
			// FIXME: Do we need to lock?
			generation := m.generationToDeploy
			deploymentResultCh := make(chan deployment.DeploymentResult)
			m.deployment = deployment.New(generation, m.deployerFunc, deploymentResultCh)
			ctx := context.TODO()
			d := m.deployment.Deploy(ctx)
			m.buildMu.Lock()
			m.deployment = d
			m.buildMu.Unlock()

			deploymentResult := <-deploymentResultCh
			logrus.Debugf("manager: deploy done with %#v", deploymentResult)

			m.buildMu.Lock()
			defer m.buildMu.Unlock()
			d = m.deployment.Update(deploymentResult)
			// The comin service is not restart by the switch-to-configuration script in order to let comin terminating properly. Instead, comin restarts itself.
			if d.RestartComin {
				m.needToBeRestarted = true
			}
			m.isRunning = false
			m.prometheus.SetDeploymentInfo(m.deployment.Generation.SelectedCommitId, deployment.StatusToString(m.deployment.Status))
			getsEvicted, evicted := m.storage.DeploymentInsertAndCommit(m.deployment)
			if getsEvicted && evicted.ProfilePath != "" {
				profile.RemoveProfilePath(evicted.ProfilePath)
			}
			m.needToReboot = utils.NeedToReboot()
			m.prometheus.SetHostInfo(m.needToReboot)
		}
	}()
}

func (m Manager) Run() {
	logrus.Info("The manager is started")
	logrus.Infof("  hostname = %s", m.hostname)
	logrus.Infof("  machineId = %s", m.machineId)
	logrus.Infof("  repositoryPath = %s", m.repositoryPath)

	m.FetchAndBuild()
	// m.Deploy()

	m.needToReboot = utils.NeedToReboot()
	m.prometheus.SetHostInfo(m.needToReboot)

	if m.needToBeRestarted {
		// TODO: stop contexts
		if err := m.cominServiceRestartFunc(); err != nil {
			logrus.Fatal(err)
			return
		}
		m.needToBeRestarted = false
	}

	<-m.stop
}
