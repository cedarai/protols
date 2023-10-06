/*
Copyright © 2023 NAME HERE <EMAIL ADDRESS>
*/
package commands

import (
	"errors"
	"os"

	"github.com/kralicky/protols/codegen"
	"github.com/kralicky/protols/pkg/sources"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// VetCmd represents the vet command
func BuildVetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vet",
		Short: "A brief description of your command",
		RunE: func(cmd *cobra.Command, args []string) error {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			driver := codegen.NewDriver(wd, zap.NewNop())
			results, err := driver.Compile(sources.SearchDirs(wd))
			if err != nil {
				return err
			}
			for _, msg := range results.Messages {
				cmd.Println(msg)
			}
			if results.Error {
				return errors.New("one or more errors occurred")
			}
			return nil
		},
	}
	return cmd
}
