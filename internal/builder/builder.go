package builder

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

type BuildResult struct {
	EndAt time.Time
	Err   error
}

type EvalResult struct {
	EndAt     time.Time
	OutPath   string
	DrvPath   string
	MachineId string
	Err       error
}

type EvalFunc func(ctx context.Context, flakeUrl string, hostname string) (drvPath string, outPath string, machineId string, err error)
type BuildFunc func(ctx context.Context, drvPath string) error

type Builder struct {
	hostname           string
	mu                 sync.Mutex
	evalFunc           EvalFunc
	buildFunc          BuildFunc
	evalTimeout        time.Duration
	buildTimeout       time.Duration
	IsEvaluating       bool
	IsBuilding         bool
	evalCancelFunc     context.CancelFunc
	buildCancelFunc    context.CancelFunc
	buildCtx           context.Context
	generation         *Generation
	previousGeneration *Generation
	EvaluationDone     chan Generation
	BuildDone          chan Generation
}

func New(hostname string, evalTimeout time.Duration, evalFunc EvalFunc, buildTimeout time.Duration, buildFunc BuildFunc) *Builder {
	return &Builder{
		hostname:     hostname,
		evalFunc:     evalFunc,
		evalTimeout:  evalTimeout,
		buildFunc:    buildFunc,
		buildTimeout: buildTimeout,
		BuildDone:    make(chan Generation),
	}
}

type Status int64

const (
	Init Status = iota
	Evaluating
	EvaluationSucceeded
	EvaluationFailed
	Building
	BuildSucceeded
	BuildFailed
)

func StatusToString(status Status) string {
	switch status {
	case Init:
		return "init"
	case Evaluating:
		return "evaluating"
	case EvaluationSucceeded:
		return "evaluation-succeeded"
	case EvaluationFailed:
		return "evaluation-failed"
	case Building:
		return "building"
	case BuildSucceeded:
		return "build-succeeded"
	case BuildFailed:
		return "build-failed"
	}
	return ""
}

func StatusFromString(status string) Status {
	switch status {
	case "init":
		return Init
	case "evaluating":
		return Evaluating
	case "evaluation-succeeded":
		return EvaluationSucceeded
	case "evaluation-failed":
		return EvaluationFailed
	case "building":
		return Building
	case "build-succeeded":
		return BuildSucceeded
	case "build-failed":
		return BuildFailed
	}
	return Init
}

// We consider each created genration is legit to be deployed: hard
// reset is ensured at RepositoryStatus creation.
type Generation struct {
	UUID     string `json:"uuid"`
	FlakeUrl string `json:"flake-url"`
	Hostname string `json:"hostname"`

	Status Status `json:"status"`

	SelectedRemoteUrl       string `json:"remote-url"`
	SelectedRemoteName      string `json:"remote-name"`
	SelectedBranchName      string `json:"branch-name"`
	SelectedCommitId        string `json:"commit-id"`
	SelectedCommitMsg       string `json:"commit-msg"`
	SelectedBranchIsTesting bool   `json:"branch-is-testing"`

	MainCommitId   string `json:"main-commit-id"`
	MainRemoteName string `json:"main-remote-name"`
	MainBranchName string `json:"main-branch-name"`

	Evaluated     bool      `json:"evaluated"`
	EvalStartedAt time.Time `json:"eval-started-at"`
	EvalEndedAt   time.Time `json:"eval-ended-at"`
	EvalErr       error     `json:"-"`
	OutPath       string    `json:"outpath"`
	DrvPath       string    `json:"drvpath"`
	MachineId     string    `json:"machine-id"`

	Built          bool      `json:"built"`
	BuildStartedAt time.Time `json:"build-started-at"`
	BuildEndedAt   time.Time `json:"build-ended-at"`
	BuildErr       error     `json:"-"`
}

func (b *Builder) GetGeneration() Generation {
	b.mu.Lock()
	defer b.mu.Unlock()
	return *b.generation
}

func (b *Builder) GetPreviousGeneration() Generation {
	b.mu.Lock()
	defer b.mu.Unlock()
	return *b.previousGeneration
}

type State struct {
	IsBuilding   bool
	IsEvaluating bool
}

func (b *Builder) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return State{
		IsBuilding:   b.IsBuilding,
		IsEvaluating: b.IsEvaluating,
	}
}

func (b *Builder) Stop() {
	b.mu.Lock()
	if b.IsEvaluating {
		b.evalCancelFunc()
		b.mu.Unlock()
		<-b.EvaluationDone
	} else {
		b.mu.Unlock()
	}
	b.mu.Lock()
	if b.IsBuilding {
		b.buildCancelFunc()
		b.mu.Unlock()
		<-b.BuildDone
	} else {
		b.mu.Unlock()
	}
}

// Eval evaluates a generation. It cancels current any generation
// evaluation or build.
func (b *Builder) Eval(flakeUrl string) {
	var ctx context.Context
	// This is to prempt the builder since we don't nede to allow
	// several evaluation in parallel
	b.Stop()
	b.mu.Lock()
	b.IsEvaluating = true
	b.previousGeneration = b.generation
	g := Generation{
		FlakeUrl: flakeUrl,
		Hostname: b.hostname,
	}
	b.generation = &g
	ctx, b.evalCancelFunc = context.WithCancel(context.Background())
	b.mu.Unlock()

	b.EvaluationDone = make(chan Generation)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, b.evalTimeout)
		defer cancel()
		drvPath, outPath, machineId, err := b.evalFunc(ctx, flakeUrl, b.hostname)
		logrus.Infof("builder: evaluation done with machineId=%d drvPath=%s", machineId, drvPath)
		b.mu.Lock()
		defer b.mu.Unlock()
		b.generation.EvalErr = err
		b.generation.DrvPath = drvPath
		b.generation.OutPath = outPath
		b.generation.MachineId = machineId
		b.generation.Evaluated = true
		b.IsEvaluating = false
		b.EvaluationDone <- *b.generation
	}()
}

// Build builds a generation.
func (b *Builder) Build() error {
	var ctx context.Context
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.generation == nil || !b.generation.Evaluated {
		return fmt.Errorf("The generation is not evaluated")
	}
	if b.IsBuilding {
		return fmt.Errorf("The builder is already building")
	}
	if b.generation.Built {
		return fmt.Errorf("The generation is already built")
	}
	b.IsBuilding = true
	ctx, b.buildCancelFunc = context.WithCancel(context.Background())

	go func() {
		ctx, cancel := context.WithTimeout(ctx, b.buildTimeout)
		defer cancel()
		err := b.buildFunc(ctx, b.generation.DrvPath)
		b.mu.Lock()
		defer b.mu.Unlock()
		b.generation.BuildErr = err
		if b.generation.BuildErr == nil {
			b.generation.Built = true
		}
		b.IsBuilding = false
		b.BuildDone <- *b.generation
	}()
	return nil
}
