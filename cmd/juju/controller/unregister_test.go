// Copyright 2016 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package controller_test

import (
	"bytes"
	"time"

	"github.com/juju/cmd"
	"github.com/juju/errors"
	jc "github.com/juju/testing/checkers"
	gc "gopkg.in/check.v1"

	"github.com/juju/juju/cmd/juju/controller"
	cmdtesting "github.com/juju/juju/cmd/testing"
	"github.com/juju/juju/jujuclient"
	"github.com/juju/juju/jujuclient/jujuclienttesting"
	"github.com/juju/juju/testing"
)

type UnregisterSuite struct {
	testing.FakeJujuXDGDataHomeSuite
	store *jujuclienttesting.MemStore
}

var _ = gc.Suite(&UnregisterSuite{})

func (s *UnregisterSuite) SetUpTest(c *gc.C) {
	s.FakeJujuXDGDataHomeSuite.SetUpTest(c)

	s.store = jujuclienttesting.NewMemStore()
	s.resetControllers()
}

func (s *UnregisterSuite) resetControllers() {
	s.store.CurrentControllerName = "fake1"
	for _, controller := range []string{"fake1", "fake2"} {
		s.store.Controllers[controller] = jujuclient.ControllerDetails{ControllerUUID: controller}
		s.store.Models[controller] = jujuclient.ControllerAccountModels{
			AccountModels: map[string]*jujuclient.AccountModels{
				"admin@local": {
					CurrentModel: "test-model1",
				},
			},
		}
		s.store.Accounts[controller] = &jujuclient.ControllerAccounts{
			Accounts: map[string]jujuclient.AccountDetails{
				"admin@local": {
					User:     "admin@local",
					Password: "password",
				},
			},
			CurrentAccount: "admin@local",
		}
	}

}

func (s *UnregisterSuite) TestInit(c *gc.C) {
	unregisterCommand := controller.NewUnregisterCommandForTest(nil)

	err := testing.InitCommand(unregisterCommand, []string{})
	c.Assert(err, gc.ErrorMatches, "controller name must be specified")

	err = testing.InitCommand(unregisterCommand, []string{"foo", "bar"})
	c.Assert(err, gc.ErrorMatches, `unrecognized args: \["bar"\]`)
}

func (s *UnregisterSuite) TestUnregisterUnknownController(c *gc.C) {
	command := controller.NewUnregisterCommandForTest(s.store)
	_, err := testing.RunCommand(c, command, "fake3")

	c.Assert(err, jc.Satisfies, errors.IsNotFound)
	c.Assert(err, gc.ErrorMatches, "controller fake3 not found")
}

func (s *UnregisterSuite) TestUnregisterCurrentController(c *gc.C) {
	command := controller.NewUnregisterCommandForTest(s.store)
	_, err := testing.RunCommand(c, command, "fake1", "-y")

	c.Assert(err, jc.ErrorIsNil)

	_, ok := s.store.Controllers["fake1"]
	c.Assert(ok, jc.IsFalse)
	_, ok = s.store.Models["fake1"]
	c.Assert(ok, jc.IsFalse)
	_, ok = s.store.Accounts["fake1"]
	c.Assert(ok, jc.IsFalse)

	c.Assert(s.store.CurrentControllerName, gc.Equals, "")
}

func (s *UnregisterSuite) TestUnregisterNonCurrentController(c *gc.C) {
	command := controller.NewUnregisterCommandForTest(s.store)
	_, err := testing.RunCommand(c, command, "fake2", "-y")
	c.Assert(err, jc.ErrorIsNil)

	_, ok := s.store.Controllers["fake1"]
	c.Assert(ok, jc.IsTrue)

	_, ok = s.store.Controllers["fake2"]
	c.Assert(ok, jc.IsFalse)
	_, ok = s.store.Models["fake2"]
	c.Assert(ok, jc.IsFalse)
	_, ok = s.store.Accounts["fake2"]
	c.Assert(ok, jc.IsFalse)

	c.Assert(s.store.CurrentControllerName, gc.Equals, "fake1")
}

var unregisterMsg = `
This command will remove connection information for controller "fake1".
Doing so will prevent you from accessing this controller until
you register it again.

Continue [y/N]?`[1:]

func (s *UnregisterSuite) TestUnregisterCommandConfirmation(c *gc.C) {
	var stdin, stdout bytes.Buffer
	ctx, err := cmd.DefaultContext()
	c.Assert(err, jc.ErrorIsNil)
	ctx.Stdout = &stdout
	ctx.Stdin = &stdin

	// Ensure confirmation is requested if "-y" is not specified.
	stdin.WriteString("n")
	_, errc := cmdtesting.RunCommand(ctx, controller.NewUnregisterCommandForTest(s.store), "fake1")
	select {
	case err := <-errc:
		c.Check(err, gc.ErrorMatches, "controller unregistration: aborted")
	case <-time.After(testing.LongWait):
		c.Fatalf("command took too long")
	}
	c.Check(testing.Stdout(ctx), gc.Equals, unregisterMsg)
	_, ok := s.store.Controllers["fake1"]
	c.Check(ok, jc.IsTrue)

	for _, answer := range []string{"y", "Y", "yes", "YES"} {
		s.resetControllers()
		stdin.Reset()
		stdout.Reset()
		stdin.WriteString(answer)
		_, errc := cmdtesting.RunCommand(ctx, controller.NewUnregisterCommandForTest(s.store), "fake1")
		select {
		case err := <-errc:
			c.Check(err, jc.ErrorIsNil)
		case <-time.After(testing.LongWait):
			c.Fatalf("command took too long")
		}
		_, ok := s.store.Controllers["fake1"]
		c.Check(ok, jc.IsFalse)
	}
}
