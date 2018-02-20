package main

import (
	"os"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"github.com/rancher/composinator/cmd"
)

var VERSION = "v0.0.0-dev"

func main() {
	app := cli.NewApp()
	app.Name = "composinator"
	app.Version = VERSION
	app.Usage = "You need help!"
	app.Action = func(c *cli.Context) error {
		logrus.Info("I'm a turkey")
		return nil
	}
	app.Commands = []cli.Command{
		cmd.UpCommand(),
	}

	if err := app.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}
