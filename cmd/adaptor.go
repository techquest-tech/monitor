/*
Copyright © 2025 NAME HERE <EMAIL ADDRESS>
*/
package main

import (
	"github.com/spf13/cobra"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/monitor/datapool"
	"github.com/techquest-tech/monitor/loki"
	"github.com/techquest-tech/monitor/messaging"
)

// adaptor represents the redis command
var adaptor = &cobra.Command{
	Use:   "adaptor",
	Short: "adaptor for messaging service",
	Run: func(cmd *cobra.Command, args []string) {
		loki.EnableLokiMonitor()
		// insights.EnabledMonitor()
		datapool.Enabled2Datapool()

		err := messaging.RunAsAdaptor()
		if err != nil {
			panic(err)
		}

		core.NotifyStarted()
		core.CloseOnlyNotified()
		core.NotifyStopping()
	},
}

func init() {
	rootCmd.AddCommand(adaptor)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// redisCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// redisCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
