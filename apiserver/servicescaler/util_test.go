// Copyright 2016 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package servicescaler_test

import (
	"github.com/juju/errors"
	jc "github.com/juju/testing/checkers"
	gc "gopkg.in/check.v1"

	"github.com/juju/juju/apiserver/common"
	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/apiserver/servicescaler"
	"github.com/juju/juju/state"
)

// mockAuth implements common.Authorizer for the tests' convenience.
type mockAuth struct {
	common.Authorizer
	modelManager bool
}

func (mock mockAuth) AuthModelManager() bool {
	return mock.modelManager
}

// auth is a convenience constructor for a mockAuth.
func auth(modelManager bool) common.Authorizer {
	return mockAuth{modelManager: modelManager}
}

// mockWatcher implements state.StringsWatcher for the tests' convenience.
type mockWatcher struct {
	state.StringsWatcher
	working bool
}

func (mock *mockWatcher) Changes() <-chan []string {
	ch := make(chan []string, 1)
	if mock.working {
		ch <- []string{"pow", "zap", "kerblooie"}
	} else {
		close(ch)
	}
	return ch
}

func (mock *mockWatcher) Err() error {
	return errors.New("blammo")
}

// watchBackend implements servicescaler.Backend for the convenience of
// the tests for the Watch method.
type watchBackend struct {
	servicescaler.Backend
	working bool
}

func (backend *watchBackend) WatchScaledServices() state.StringsWatcher {
	return &mockWatcher{working: backend.working}
}

// watchFixture collects components needed to test the Watch method.
type watchFixture struct {
	Facade    *servicescaler.Facade
	Resources *common.Resources
}

func newWatchFixture(c *gc.C, working bool) *watchFixture {
	backend := &watchBackend{working: working}
	resources := common.NewResources()
	facade, err := servicescaler.NewFacade(backend, resources, auth(true))
	c.Assert(err, jc.ErrorIsNil)
	return &watchFixture{facade, resources}
}

// rescaleBackend implements servicescaler.Backend for the convenience of
// the tests for the Rescale method.
type rescaleBackend struct {
	servicescaler.Backend
}

func (rescaleBackend) RescaleService(name string) error {
	switch name {
	case "expected":
		return nil
	case "missing":
		return errors.NotFoundf("service")
	default:
		return errors.New("blammo")
	}
}

// rescaleFixture collects components needed to test the Rescale method.
type rescaleFixture struct {
	Facade *servicescaler.Facade
}

func newRescaleFixture(c *gc.C) *rescaleFixture {
	facade, err := servicescaler.NewFacade(rescaleBackend{}, nil, auth(true))
	c.Assert(err, jc.ErrorIsNil)
	return &rescaleFixture{facade}
}

// entities is a convenience constructor for params.Entities.
func entities(tags ...string) params.Entities {
	entities := params.Entities{Entities: make([]params.Entity, len(tags))}
	for i, tag := range tags {
		entities.Entities[i].Tag = tag
	}
	return entities
}
