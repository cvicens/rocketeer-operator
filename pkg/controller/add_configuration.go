package controller

import (
	"github.com/cvicens/rocketeer-operator/pkg/controller/configuration"
)

func init() {
	// AddToManagerFuncs is a list of functions to create controllers and add them to a manager.
	AddToManagerFuncs = append(AddToManagerFuncs, configuration.Add)
}
