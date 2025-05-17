package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync/atomic"

	"github.com/caddyserver/xcaddy"
)

var (
	// The number of jobs that are currently running
	// NOTE: This is very simplistic and we most definitely can/should
	// migrate to a more robust worker pool implementation in the future.
	runningJobCount atomic.Int32
)

// Job is the structure of a job to be processed by the server. This is
// generally serialized into JSON and queued into Redis.
type Job struct {
	// OS is the operating system to build for
	OS string `json:"os"`
	// Arch is the architecture to build for
	Arch string `json:"arch"`
	// Packages is the list of caddy modules to build
	Packages []string `json:"packages"`
	// ID is the unique identifier for the job
	ID string `json:"id"`
	// BinaryOutputPath is the path and name to save the compiled binary as
	BinaryOutputPath string `json:"binary_output_path"`
	// CaddyVersion is the version of caddy to build
	CaddyVersion string `json:"caddy_version"`
}

// processJobWithLogging is a helper function that processes a job and
// logs the output to the websocket.
func processJobWithLogging(ctx context.Context, job Job) (string, error) {
	runningJobCount.Add(1)
	defer runningJobCount.Add(-1)
	var plugins []xcaddy.Dependency
	// Send initial status
	sendMessage(ctx, MessageTypeBuildStarted, job.ID, "Job started")
	sendMessage(ctx, MessageTypeLog, job.ID, fmt.Sprintf("Starting job processing for %s %s", job.OS, job.Arch))

	for _, plugin := range job.Packages {
		module, version, _, err := splitWith(plugin)
		if err != nil {
			log.Printf("Error splitting plugin: %v", err)
			continue
		}
		module = strings.TrimSuffix(module, "/")

		plugins = append(plugins, xcaddy.Dependency{
			PackagePath: module,
			Version:     version,
		})
	}
	builder := xcaddy.Builder{
		Compile: xcaddy.Compile{
			Platform: xcaddy.Platform{
				OS:   job.OS,
				Arch: job.Arch,
			},
		},
		CaddyVersion: job.CaddyVersion,
		Plugins:      plugins,
	}

	// Pipe the log output to the websocket
	wsLogWriter := &websocketLogWriter{jobID: job.ID}
	wsLogger := log.New(wsLogWriter, "", 0)
	loggerKey := xcaddy.LoggerKeyType{}
	ctx = context.WithValue(ctx, loggerKey, wsLogger)

	err := builder.Build(ctx, job.BinaryOutputPath)
	if err != nil {
		log.Printf("Error building caddy: %v", err)
		sendMessage(ctx, MessageTypeBuildFailed, job.ID, fmt.Sprintf("Error building caddy: %v", err))
		return "", err
	}

	sendMessage(ctx, MessageTypeLog, job.ID, "Job completed successfully")
	sendMessage(ctx, MessageTypeBuildSuccess, job.ID, job.BinaryOutputPath)
	return job.BinaryOutputPath, nil
}
