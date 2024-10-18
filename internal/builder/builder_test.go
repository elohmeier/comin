package builder

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

var mkNixEvalMock = func(evalDone chan struct{}) EvalFunc {
	return func(ctx context.Context, repositoryPath string, hostname string) (string, string, string, error) {
		select {
		case <-ctx.Done():
			return "", "", "", ctx.Err()
		case <-evalDone:
			return "drv-path", "out-path", "", nil
		}
	}
}

var mkNixBuildMock = func(buildDone chan struct{}) BuildFunc {
	return func(ctx context.Context, drvPath string) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-buildDone:
			return nil
		}
	}
}

var nixBuildMockNil = func(ctx context.Context, drvPath string) error { return nil }

func TestBuilderBuild(t *testing.T) {
	evalDone := make(chan struct{})
	buildDone := make(chan struct{})

	b := New("my-machine", 2*time.Second, mkNixEvalMock(evalDone), 2*time.Second, mkNixBuildMock(buildDone))
	assert.ErrorContains(t, b.Build(), "The generation is not evaluated")

	b.Eval("flake-url-1")
	close(evalDone)
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.False(c, b.IsEvaluating)
	}, 2*time.Second, 100*time.Millisecond)

	b.Build()
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.True(c, b.IsBuilding)
	}, 2*time.Second, 100*time.Millisecond)
	err := b.Build()
	assert.ErrorContains(t, err, "The builder is already building")

	// Stop the builder
	b.Stop()
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.False(c, b.IsBuilding)
		g := b.GetGeneration()
		assert.False(c, g.Built)
		assert.ErrorContains(c, g.BuildErr, "context canceled")
	}, 2*time.Second, 100*time.Millisecond)

	// The builder timeouts
	err = b.Build()
	assert.Nil(t, err)
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.False(c, b.IsBuilding)
		g := b.GetGeneration()
		assert.False(c, g.Built)
		assert.ErrorContains(c, g.BuildErr, "context deadline exceeded")
	}, 3*time.Second, 100*time.Millisecond)

	// The builder succeeds
	err = b.Build()
	assert.Nil(t, err)
	close(buildDone)
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.False(c, b.IsBuilding)
		g := b.GetGeneration()
		assert.True(c, g.Built)
		assert.Nil(c, g.BuildErr)
	}, 3*time.Second, 100*time.Millisecond)

	// The generation is already built
	err = b.Build()
	assert.ErrorContains(t, err, "The generation is already built")
}

func TestEval(t *testing.T) {
	evalDone := make(chan struct{})
	b := New(5*time.Second, mkNixEvalMock(evalDone), 5*time.Second, nixBuildMockNil)
	b.Eval("flake-url-1", "my-machine")
	assert.True(t, b.IsEvaluating)

	close(evalDone)
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.False(c, b.IsEvaluating)
		g := b.GetGeneration()
		assert.True(c, g.Evaluated)
		assert.Equal(c, "drv-path", g.DrvPath)
		assert.Equal(c, "out-path", g.OutPath)
	}, 2*time.Second, 100*time.Millisecond)
}

func TestBuilderPremption(t *testing.T) {
	evalDone := make(chan struct{})
	b := New(5*time.Second, mkNixEvalMock(evalDone), 5*time.Second, nixBuildMockNil)
	b.Eval("flake-url-1", "my-machine")
	assert.True(t, b.IsEvaluating)

	b.Eval("flake-url-2", "my-machine")
	assert.True(t, b.IsEvaluating)

	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		g := b.GetPreviousGeneration()
		assert.ErrorContains(c, g.EvalErr, "context canceled")
	}, 2*time.Second, 100*time.Millisecond, "builder cancel didn't work")
}

func TestBuilderStop(t *testing.T) {
	evalDone := make(chan struct{})
	b := New(5*time.Second, mkNixEvalMock(evalDone), 5*time.Second, nixBuildMockNil)
	b.Eval("flake-url-1", "my-machine")
	assert.True(t, b.IsEvaluating)
	b.Stop()
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		g := b.GetGeneration()
		assert.ErrorContains(c, g.EvalErr, "context canceled")
	}, 2*time.Second, 100*time.Millisecond, "builder cancel didn't work")
}

func TestBuilderTimeout(t *testing.T) {
	evalDone := make(chan struct{})
	b := New(1*time.Second, mkNixEvalMock(evalDone), 5*time.Second, nixBuildMockNil)
	b.Eval("flake-url-1", "my-machine")
	assert.True(t, b.IsEvaluating)
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		g := b.GetGeneration()
		assert.ErrorContains(c, g.EvalErr, "context deadline exceeded")
	}, 3*time.Second, 100*time.Millisecond, "builder timeout didn't work")
}
