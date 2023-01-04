package main

import (
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/guseggert/clustertest/agent"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap/zapcore"
)

func main() {
	app := &cli.App{
		Name:  "nodeagent",
		Usage: "the node agent for managing cluster nodes",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "on-heartbeat-failure",
				Usage: "Action to take on a heartbeat failure. One of [shutdown,exit,none].",
				Value: "none",
			},
			&cli.StringFlag{
				Name:  "heartbeat-timeout",
				Usage: "Duration to wait for a heartbeat before shutting down.",
				Value: "1m",
			},
			&cli.StringFlag{
				Name:  "listen-addr",
				Usage: "The address for the HTTP server to listen on.",
				Value: "0.0.0.0:8080",
			},
			&cli.StringFlag{
				Name:     "ca-cert-pem",
				Usage:    "The CA cert PEM bytes to use (base64-encoded).",
				Required: true,
			},
			&cli.StringFlag{
				Name:     "cert-pem",
				Usage:    "The cert PEM bytes to use (base64-encoded).",
				Required: true,
			},
			&cli.StringFlag{
				Name:     "key-pem",
				Usage:    "The key PEM bytes to use (base64-encoded).",
				Required: true,
			},
		},
		Action: func(ctx *cli.Context) error {
			onHeartbeatFailure := ctx.String("on-heartbeat-failure")
			heartbeatTimeoutStr := ctx.String("heartbeat-timeout")
			listenAddr := ctx.String("listen-addr")
			caCertPEMEncoded := ctx.String("ca-cert-pem")
			certPEMEncoded := ctx.String("cert-pem")
			keyPEMEncoded := ctx.String("key-pem")

			caCertPEMBytes, err := base64.StdEncoding.DecodeString(caCertPEMEncoded)
			if err != nil {
				return fmt.Errorf("decoding CA cert PEM: %w", err)
			}
			certPEMBytes, err := base64.StdEncoding.DecodeString(certPEMEncoded)
			if err != nil {
				return fmt.Errorf("decoding cert PEM: %w", err)
			}
			keyPEMBytes, err := base64.StdEncoding.DecodeString(keyPEMEncoded)
			if err != nil {
				return fmt.Errorf("decoding key PEM: %w", err)
			}

			var heartbeatFailureHandler func()
			switch onHeartbeatFailure {
			case "shutdown":
				heartbeatFailureHandler = agent.HeartbeatFailureShutdown
			case "exit":
				heartbeatFailureHandler = agent.HeartbeatFailureExit
			case "none":
				// nothing
			default:
				return fmt.Errorf("unsupported on-heartbeat-failure %q", onHeartbeatFailure)
			}

			heartbeatTimeout, err := time.ParseDuration(heartbeatTimeoutStr)
			if err != nil {
				return fmt.Errorf("parsing heartbeat timeout: %w", err)
			}

			agent, err := agent.NewNodeAgent(
				caCertPEMBytes,
				certPEMBytes,
				keyPEMBytes,
				agent.WithLogLevel(zapcore.DebugLevel),
				agent.WithHeartbeatTimeout(heartbeatTimeout),
				agent.WithListenAddr(listenAddr),
				agent.WithHeartbeatFailureHandler(heartbeatFailureHandler),
			)
			if err != nil {
				return fmt.Errorf("building agent: %w", err)
			}

			err = agent.Run()
			if err != nil {
				if err != http.ErrServerClosed {
					return err
				}
			}

			return nil
		},
	}
	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}
