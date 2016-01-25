// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package controller

import (
	"strings"

	"github.com/juju/cmd"
	"github.com/juju/errors"
	"github.com/juju/names"
	"launchpad.net/gnuflag"

	"github.com/juju/juju/api/base"
	"github.com/juju/juju/cmd/envcmd"
	"github.com/juju/juju/environs/configstore"
)

// NewUseEnvironmentCommand returns a command that caches information
// about an environment the user can use in the controller locally.
func NewUseEnvironmentCommand() cmd.Command {
	return envcmd.WrapController(&useEnvironmentCommand{})
}

// useEnvironmentCommand returns the list of all the environments the
// current user can access on the current controller.
type useEnvironmentCommand struct {
	envcmd.ControllerCommandBase

	api       UseModelAPI
	userCreds *configstore.APICredentials
	endpoint  *configstore.APIEndpoint

	LocalName string
	Owner     string
	EnvName   string
	EnvUUID   string
}

// UseModelAPI defines the methods on the environment manager API that
// the use environment command calls.
type UseModelAPI interface {
	Close() error
	ListModels(user string) ([]base.UserModel, error)
}

var useEnvDoc = `
use-model caches the necessary information about the specified
model on the current machine. This allows you to switch between
models.

By default, the local names for the model are based on the name that the
owner of the model gave it when they created it.  If you are the owner
of the model, then the local name is just the name of the model.
If you are not the owner, the name is prefixed by the name of the owner and a
dash.

If there is just one model called "test" in the current controller that you
have access to, then you can just specify the name.

    $ juju use-model test

If however there are multiple models called "test" that are owned

    $ juju use-model test
    Multiple models matched name "test":
      cb4b94e8-29bb-44ae-820c-adac21194395, owned by bob@local
      ae673c19-73ef-437f-8224-4842a1772bdf, owned by mary@local
    Please specify either the model UUID or the owner to disambiguate.
    ERROR multiple models matched

You can specify either the model UUID like this:

    $ juju use-model cb4b94e8-29bb-44ae-820c-adac21194395

Or, specify the owner:

    $ juju use-model mary@local/test

Since '@local' is the default for users, this can be shortened to:

    $ juju use-model mary/test


See Also:
    juju help controllers
    juju help create-model
    juju help model share
    juju help model unshare
    juju help switch
    juju help add-user
`

// Info implements Command.Info
func (c *useEnvironmentCommand) Info() *cmd.Info {
	return &cmd.Info{
		Name:    "use-model",
		Purpose: "use an model that you have access to on the controller",
		Doc:     useEnvDoc,
	}
}

func (c *useEnvironmentCommand) getAPI() (UseModelAPI, error) {
	if c.api != nil {
		return c.api, nil
	}
	return c.NewModelManagerAPIClient()
}

func (c *useEnvironmentCommand) getConnectionCredentials() (configstore.APICredentials, error) {
	if c.userCreds != nil {
		return *c.userCreds, nil
	}
	return c.ConnectionCredentials()
}

func (c *useEnvironmentCommand) getConnectionEndpoint() (configstore.APIEndpoint, error) {
	if c.endpoint != nil {
		return *c.endpoint, nil
	}
	return c.ConnectionEndpoint()
}

// SetFlags implements Command.SetFlags.
func (c *useEnvironmentCommand) SetFlags(f *gnuflag.FlagSet) {
	f.StringVar(&c.LocalName, "name", "", "the local name for this model")
}

// SetFlags implements Command.Init.
func (c *useEnvironmentCommand) Init(args []string) error {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return errors.New("no model supplied")
	}

	name, args := args[0], args[1:]

	// First check to see if an owner has been specified.
	bits := strings.SplitN(name, "/", 2)
	switch len(bits) {
	case 1:
		// No user specified
		c.EnvName = bits[0]
	case 2:
		owner := bits[0]
		if names.IsValidUser(owner) {
			c.Owner = owner
		} else {
			return errors.Errorf("%q is not a valid user", owner)
		}
		c.EnvName = bits[1]
	}

	// Environment names can generally be anything, but we take a good
	// stab at trying to determine if the user has specified a UUID
	// instead of a name. For now, we only accept a properly formatted UUID,
	// which means one with dashes in the right place.
	if names.IsValidEnvironment(c.EnvName) {
		c.EnvUUID, c.EnvName = c.EnvName, ""
	}

	return cmd.CheckEmpty(args)
}

// Run implements Command.Run
func (c *useEnvironmentCommand) Run(ctx *cmd.Context) error {
	client, err := c.getAPI()
	if err != nil {
		return errors.Trace(err)
	}
	defer client.Close()

	creds, err := c.getConnectionCredentials()
	if err != nil {
		return errors.Trace(err)
	}
	endpoint, err := c.getConnectionEndpoint()
	if err != nil {
		return errors.Trace(err)
	}

	username := names.NewUserTag(creds.User).Canonical()

	env, err := c.findMatchingEnvironment(ctx, client, creds)
	if err != nil {
		return errors.Trace(err)
	}

	if c.LocalName == "" {
		if env.Owner == username {
			c.LocalName = env.Name
		} else {
			envOwner := names.NewUserTag(env.Owner)
			c.LocalName = envOwner.Name() + "-" + env.Name
		}
	}

	// Check with the store to see if we have an environment with that name.
	store, err := configstore.Default()
	if err != nil {
		return errors.Trace(err)
	}

	existing, err := store.ReadInfo(c.LocalName)
	if err == nil {
		// We have an existing environment with the same name. If it is the
		// same environment with the same user, then this is fine, and we just
		// change the current environment.
		endpoint := existing.APIEndpoint()
		existingCreds := existing.APICredentials()
		// Need to make sure we check the username of the credentials,
		// not just matching tags.
		existingUsername := names.NewUserTag(existingCreds.User).Canonical()
		if endpoint.EnvironUUID == env.UUID && existingUsername == username {
			ctx.Infof("You already have model details for %q cached locally.", c.LocalName)
			return envcmd.SetCurrentEnvironment(ctx, c.LocalName)
		}
		ctx.Infof("You have an existing model called %q, use --name to specify a different local name.", c.LocalName)
		return errors.New("existing model")
	}

	info := store.CreateInfo(c.LocalName)
	if err := c.updateCachedInfo(info, env.UUID, creds, endpoint); err != nil {
		return errors.Annotatef(err, "failed to cache model details")
	}

	return envcmd.SetCurrentEnvironment(ctx, c.LocalName)
}

func (c *useEnvironmentCommand) updateCachedInfo(info configstore.EnvironInfo, envUUID string, creds configstore.APICredentials, endpoint configstore.APIEndpoint) error {
	info.SetAPICredentials(creds)
	// Specify the environment UUID. The server UUID will be the same as the
	// endpoint that we have just connected to, as will be the CACert, addresses
	// and hostnames.
	endpoint.EnvironUUID = envUUID
	info.SetAPIEndpoint(endpoint)
	return errors.Trace(info.Write())
}

func (c *useEnvironmentCommand) findMatchingEnvironment(ctx *cmd.Context, client UseModelAPI, creds configstore.APICredentials) (base.UserModel, error) {

	var empty base.UserModel

	envs, err := client.ListModels(creds.User)
	if err != nil {
		return empty, errors.Annotate(err, "cannot list models")
	}

	var owner string
	if c.Owner != "" {
		// The username always contains the provider aspect of the user.
		owner = names.NewUserTag(c.Owner).Canonical()
	}

	// If we have a UUID, we warn if the owner is different, but accept it.
	// We also trust that the environment UUIDs are unique
	if c.EnvUUID != "" {
		for _, env := range envs {
			if env.UUID == c.EnvUUID {
				if owner != "" && env.Owner != owner {
					ctx.Infof("Specified model owned by %s, not %s", env.Owner, owner)
				}
				return env, nil
			}
		}
		return empty, errors.NotFoundf("matching model")
	}

	var matches []base.UserModel
	for _, env := range envs {
		match := env.Name == c.EnvName
		if match && owner != "" {
			match = env.Owner == owner
		}
		if match {
			matches = append(matches, env)
		}
	}

	// If there is only one match, that's the one.
	switch len(matches) {
	case 0:
		return empty, errors.NotFoundf("matching model")
	case 1:
		return matches[0], nil
	}

	// We are going to return an error, but tell the user what the matches
	// were so they can make an informed decision. We are also going to assume
	// here that the resulting environment list has only one matching name for
	// each user. There are tests creating environments that enforce this.
	ctx.Infof("Multiple models matched name %q:", c.EnvName)
	for _, env := range matches {
		ctx.Infof("  %s, owned by %s", env.UUID, env.Owner)
	}
	ctx.Infof("Please specify either the model UUID or the owner to disambiguate.")

	return empty, errors.New("multiple models matched")
}