package convert

import (
	"fmt"

	"github.com/docker-slim/docker-slim/internal/app/master/commands"

	"github.com/urfave/cli"
)

var (
	Name  = "convert"
	Usage = "Convert container image"
	Alias = "k"
)

var CLI = cli.Command{
	Name:    Name,
	Aliases: []string{Alias},
	Usage:   Usage,
	Action: func(ctx *cli.Context) error {
		commands.ShowCommunityInfo()
		if len(ctx.Args()) < 1 {
			fmt.Printf("docker-slim[%s]: missing target info...\n\n", Name)
			cli.ShowCommandHelp(ctx, Name)
			return nil
		}

		gcvalues, err := commands.GlobalCommandFlagValues(ctx)
		if err != nil {
			return err
		}

		targetRef := ctx.Args().First()

		ec := &commands.ExecutionContext{}

		OnCommand(
			gcvalues,
			targetRef,
			ec)
		commands.ShowCommunityInfo()
		return nil
	},
}
