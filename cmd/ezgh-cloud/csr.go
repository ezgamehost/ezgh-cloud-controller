package main

import (
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/csr"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/run"
)

func init() { subcommands["csr-approver"] = runCSRApprover }

func runCSRApprover([]string) error {
	env, err := run.Manager("csr-approver")
	if err != nil {
		return err
	}
	a := &csr.Approver{
		Client: env.Manager.GetClient(), Clients: env.Clients, ConfigPath: env.ConfigPath,
		Recorder: env.Recorder("ezgh-cloud-csr-approver"), Log: env.Log,
	}
	if err := a.SetupWithManager(env.Manager); err != nil {
		return err
	}
	return env.Start()
}
