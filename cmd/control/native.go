package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/gabrielpgava/newempire-server-control/internal/config"
	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/instances"
	"github.com/gabrielpgava/newempire-server-control/internal/secrets"
	"github.com/gabrielpgava/newempire-server-control/internal/servicehelper"
)

func runtimeHelper(args []string) error {
	return servicehelper.Run(context.Background(), args, os.Stdin, os.Stdout)
}
func installService(c config.Config, args []string) error {
	if len(args) == 0 {
		return servicehelper.InstallService(context.Background(), os.Stdin)
	}
	if len(args) != 1 {
		return errors.New("install-service accepts one instance ID or one JSON registration on standard input")
	}
	db, err := database.Open(filepath.Join(c.DataDir, "panel.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	vault, err := secrets.Open(filepath.Join(c.DataDir, "master.key"))
	if err != nil {
		return err
	}
	service := instances.Service{DB: db, Vault: vault, Config: c}
	i, err := service.Get(context.Background(), args[0])
	if err != nil {
		return err
	}
	body, err := json.Marshal(i)
	if err != nil {
		return err
	}
	return servicehelper.InstallService(context.Background(), strings.NewReader(string(body)))
}
