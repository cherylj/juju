// Copyright 2016 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package resourceadapters

import (
	"io"

	"github.com/juju/errors"
	"github.com/juju/names"
	"gopkg.in/juju/charm.v6-unstable"

	"github.com/juju/juju/resource"
	"github.com/juju/juju/resource/charmstore"
	corestate "github.com/juju/juju/state"
)

// resourceOpener is an implementation of server.ResourceOpener.
type resourceOpener struct {
	st     corestate.Resources
	userID names.Tag
	unit   resource.Unit
}

// OpenResource implements server.ResourceOpener.
func (ro *resourceOpener) OpenResource(name string) (resource.Opened, error) {
	if ro.unit == nil {
		return resource.Opened{}, errors.Errorf("missing unit")
	}
	cURL, _ := ro.unit.CharmURL()

	ops, err := ro.newCSOps(cURL)
	if err != nil {
		return resource.Opened{}, errors.Trace(err)
	}

	res, reader, err := ops.GetResource(cURL, name)
	if err != nil {
		return resource.Opened{}, errors.Trace(err)
	}

	opened := resource.Opened{
		Resource:   res,
		ReadCloser: reader,
	}
	return opened, nil
}

type csOps interface {
	// GetResource returns a reader for the resource's data. That data
	// is streamed from the charm store. It will also be stored in
	// the cache, if one is set up.
	GetResource(cURL *charm.URL, name string) (resource.Resource, io.ReadCloser, error)
}

func (ro resourceOpener) newCSOps(cURL *charm.URL) (csOps, error) {
	deps := newCharmstoreOpener(cURL)
	cache := &charmstoreEntityCache{
		st:        ro.st,
		userID:    ro.userID,
		unit:      ro.unit,
		serviceID: ro.unit.ServiceName(),
	}
	ops, err := charmstore.NewOperations(deps, cache)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return ops, nil
}
