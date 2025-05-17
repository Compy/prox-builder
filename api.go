package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	osPkg "os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Embed the UI located at ui/dist into the binary
//
//go:embed ui/dist
var embeddedUI embed.FS

// WebsocketMessage is the structure of a message to be sent to the websocket and
// processed by the frontend javascript. We usually use this for sending build
// logs back to the client.
type WebsocketMessage struct {
	MessageType string `json:"message_type"`
	Message     string `json:"message"`
}

// websocketLogWriter is a simple io.Writer implementation used to redirect
// the core log output to the websocket.
// In agent mode, this redirects logs to a redis pubsub channel, which the
// hub can then relay to the websocket.
type websocketLogWriter struct {
	jobID string
}

func (w *websocketLogWriter) Write(p []byte) (n int, err error) {
	sendMessage(context.Background(), MessageTypeLog, w.jobID, string(p))
	return len(p), nil
}

// MessageType is the type of message to be sent to the websocket.
type MessageType string

const (
	// MessageTypeLog is a log message from the build
	MessageTypeLog MessageType = "log"
	// MessageTypeBuildStarted is a message sent when the build starts
	MessageTypeBuildStarted MessageType = "build_started"
	// MessageTypeBuildFailed is a message sent when the build fails
	// The message will contain the error message
	MessageTypeBuildFailed MessageType = "build_failed"
	// MessageTypeBuildSuccess is a message sent when the build succeeds.
	// The message will contain the URL to download the build.
	MessageTypeBuildSuccess MessageType = "build_success"
)

var (
	// The websocket upgrader for the embedded HTTP server
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true // Allow all origins for now
		},
	}
	// The map of websocket connections that are currently active
	clients = make(map[string]*websocket.Conn)
	// A mutex for safely accessing the active websocket clients map
	clientsLock sync.RWMutex
	// A list of job IDs that are currently running
	runningJobs = make(map[string]bool)
	// A mutex for safely accessing the running jobs map
	runningJobsLock sync.RWMutex
)

// This is a handler for our websocket upgrader. This allows us to
// upgrade a normal HTTP connection to a websocket connection inline with
// our API webserver instead of having to spin up a different listener.
// When the client lands here, we add the websocket connection to the map
// of active connections and then close it when the client disconnects.
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		http.Error(w, "job_id is required", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Error upgrading to WebSocket: %v", err)
		return
	}

	clientsLock.Lock()
	clients[jobID] = conn
	clientsLock.Unlock()

	defer func() {
		clientsLock.Lock()
		delete(clients, jobID)
		clientsLock.Unlock()
		conn.Close()
	}()

	// If the job_id is 'test', then send a message to the client every second
	if jobID == "test" {
		go func() {
			for {
				// If the connection is closed, break out of the loop
				if !isWebSocketClientConnected(jobID) {
					log.Printf("WebSocket connection closed for job %s", jobID)
					break
				}
				sendMessage(context.Background(), MessageTypeLog, jobID, "Hello, world!")
				time.Sleep(1 * time.Second)
			}
		}()
	}

	// Keep the connection alive
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}
}

// handleDownload is a handler for the /api/download endpoint. This is
// used to download a build from the server. It is designed to be
// api-compatible with the Caddy download endpoint. All of the queued job
// logic starts here.
//
// This endpoint can be invoked by a GET or a HEAD request (as the Caddy
// download endpoint does). In order to retain compatibility, we need to
// hold this endpoint open until the build is complete and then send the
// build artifact to the client.
func handleDownload(w http.ResponseWriter, r *http.Request) {
	// Parse the request parameters
	os := r.URL.Query().Get("os")
	arch := r.URL.Query().Get("arch")
	packages := r.URL.Query()["p"]
	jobID := r.URL.Query().Get("job_id")
	caddyVersion := r.URL.Query().Get("caddy_version")
	outputFileName := fmt.Sprintf("caddy_%s_%s_custom", os, arch)
	if os == "windows" {
		outputFileName += ".exe"
	}

	// Generate a random job ID if the job ID is not provided
	if jobID == "" {
		jobID = uuid.New().String()
	}

	// Validate the request parameters
	if os == "" || arch == "" {
		http.Error(w, "os and arch are required", http.StatusBadRequest)
		return
	}

	// Check if the job is already running
	runningJobsLock.RLock()
	_, exists := runningJobs[jobID]
	runningJobsLock.RUnlock()
	if exists {
		http.Error(w, "job already running", http.StatusConflict)
		return
	}
	runningJobsLock.Lock()
	runningJobs[jobID] = true
	runningJobsLock.Unlock()

	// Create job
	job := Job{
		OS:           os,
		Arch:         arch,
		Packages:     packages,
		ID:           jobID,
		CaddyVersion: caddyVersion,
	}

	cacheKey := calculateCacheKey(job)
	job.BinaryOutputPath = fmt.Sprintf("%s/%s", config.BinaryPath, cacheKey)

	// If the artifact already exists, we can serve it directly
	if _, err := osPkg.Stat(job.BinaryOutputPath); err == nil {
		log.Printf("Serving cached build artifact os=%s arch=%s job_id=%s cache_key=%s", job.OS, job.Arch, jobID, cacheKey)
		sendMessage(context.Background(), MessageTypeBuildSuccess, job.ID, "The build was completed successfully (cached)")
		runningJobsLock.Lock()
		delete(runningJobs, jobID)
		runningJobsLock.Unlock()
		w.Header().Add("Content-Type", "application/octet-stream")
		w.Header().Add("Content-Disposition", fmt.Sprintf("attachment; filename=%s", outputFileName))
		http.ServeFile(w, r, job.BinaryOutputPath)
		return
	}

	log.Printf("Cache key for job %s: %s", jobID, cacheKey)
	var buildResult string
	var buildError error

	// If we're in hub mode, then we need to enqueue the job in Redis
	if config.IsHub {
		jobJSON, err := json.Marshal(job)
		if err != nil {
			log.Printf("Error marshalling job: %v", err)
			http.Error(w, "error marshalling job", http.StatusInternalServerError)
			return
		}
		result := rdb.LPush(context.Background(), "jobs", jobJSON)
		if result.Err() != nil {
			log.Printf("Error enqueuing job: %v", result.Err())
			http.Error(w, "error enqueuing job", http.StatusInternalServerError)
			return
		}

		// Now we need to listen for logs from the agent via redis pubsub and relay them to the websocket
		// This process will block until the build is complete, at which point we can serve the build artifact
		buildResult, buildError = relayLogsFromRedisToWebSocket(context.Background(), jobID)
	} else {
		// We're running in standalone mode, process the job directly on this node
		buildResult, buildError = processJobWithLogging(context.Background(), job)
	}

	runningJobsLock.Lock()
	delete(runningJobs, jobID)
	runningJobsLock.Unlock()

	if buildError != nil {
		http.Error(w, buildError.Error(), http.StatusInternalServerError)
		return
	}

	// Serve the build artifact
	w.Header().Add("Content-Type", "application/octet-stream")
	w.Header().Add("Content-Disposition", fmt.Sprintf("attachment; filename=%s", outputFileName))
	http.ServeFile(w, r, buildResult)
}

// splitWith splits a string into a module, version, and replace string.
// This function was taken directly from xcaddy.
func splitWith(arg string) (module, version, replace string, err error) {
	const versionSplit, replaceSplit = "@", "="

	parts := strings.SplitN(arg, replaceSplit, 2)
	if len(parts) > 1 {
		replace = parts[1]
	}
	module = parts[0]

	// accommodate module paths that have @ in them, but we can only tolerate that if there's also
	// a version, otherwise we don't know if it's a version separator or part of the file path (see #109)
	lastVersionSplit := strings.LastIndex(module, versionSplit)
	if lastVersionSplit < 0 {
		if replaceIdx := strings.Index(module, replaceSplit); replaceIdx >= 0 {
			module, replace = module[:replaceIdx], module[replaceIdx+1:]
		}
	} else {
		module, version = module[:lastVersionSplit], module[lastVersionSplit+1:]
		if replaceIdx := strings.Index(version, replaceSplit); replaceIdx >= 0 {
			version, replace = module[:replaceIdx], module[replaceIdx+1:]
		}
	}

	if module == "" {
		err = fmt.Errorf("module name is required")
	}

	return
}

func isWebSocketClientConnected(jobID string) bool {
	clientsLock.RLock()
	conn, exists := clients[jobID]
	clientsLock.RUnlock()
	return exists && conn != nil
}

// Calculate a cache key for the given job. To do this reliably, we need to
// sort the packages and then concatenate them together, and then sha256 hash
// the whole thing.
func calculateCacheKey(job Job) string {
	sort.Strings(job.Packages)
	packageString := strings.Join(job.Packages, ",")
	caddyVersion := job.CaddyVersion

	// If the caddy version is not specified, or is "latest", then use the latest commit SHA
	// This helps bust cache when the latest caddy commit changes
	if caddyVersion == "" || caddyVersion == "latest" {
		caddyVersion = caddyCommitSHA
	}

	hash := sha256.Sum256([]byte(packageString + "|" + job.OS + "|" + job.Arch + "|" + caddyVersion))
	return hex.EncodeToString(hash[:])
}

// Send a generic message to the log output. This function automatically handles
// the message routing if we call this from the remote agent or the local hub.
// If we're in agent mode, then we send the message to a redis pubsub channel to
// be relayed to the hub, otherwise we send it to the websocket clients directly.
func sendMessage(ctx context.Context, messageType MessageType, jobID string, message string) {
	msg := WebsocketMessage{
		MessageType: string(messageType),
		Message:     message,
	}
	msgJSON, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshalling message: %v", err)
		return
	}
	// If we're in agent mode, then we need to send the message to
	// a redis pubsub channel, otherwise we send it to the websocket
	// clients.
	if config.IsAgent {
		rdb.Publish(ctx, fmt.Sprintf("job:%s", jobID), msgJSON)
	} else {
		sendWebSocketMessage(jobID, msgJSON)
	}
}

// This function sends a serialized message to the websocket for the given
// job ID.
func sendWebSocketMessage(jobID string, jsonMessage []byte) {
	clientsLock.RLock()
	conn, exists := clients[jobID]
	clientsLock.RUnlock()

	if exists {
		err := conn.WriteMessage(websocket.TextMessage, jsonMessage)
		if err != nil {
			log.Printf("Error sending message to WebSocket: %v", err)
		}
	}
}

// This function listens for messages from the redis pubsub channel for
// the given job ID and relays any messages to the websocket.
// It will also break out of the loop and close the channel when the build
// succeeds or fails.
func relayLogsFromRedisToWebSocket(ctx context.Context, jobID string) (string, error) {
	pubsub := rdb.Subscribe(ctx, fmt.Sprintf("job:%s", jobID))
	defer pubsub.Close()
	var buildError error = nil
	var buildResult string
	for msg := range pubsub.Channel() {
		// Deserialize the message payload
		var wsMsg WebsocketMessage
		err := json.Unmarshal([]byte(msg.Payload), &wsMsg)
		if err != nil {
			log.Printf("Error unmarshalling message: %v", err)
			continue
		}
		if wsMsg.MessageType == string(MessageTypeLog) {
			sendWebSocketMessage(jobID, []byte(msg.Payload))
		} else if wsMsg.MessageType == string(MessageTypeBuildSuccess) || wsMsg.MessageType == string(MessageTypeBuildFailed) {
			sendWebSocketMessage(jobID, []byte(msg.Payload))
			if wsMsg.MessageType == string(MessageTypeBuildSuccess) {
				buildError = nil
				buildResult = wsMsg.Message
			} else {
				buildError = fmt.Errorf(wsMsg.Message)
			}
			// These messages are the last ones we'll send, so we can break
			// out of the loop and close the channel.
			break
		}
	}
	log.Printf("Relaying logs from Redis to WebSocket for job %s completed. Closing channel.", jobID)
	runningJobsLock.Lock()
	delete(runningJobs, jobID)
	runningJobsLock.Unlock()
	return buildResult, buildError
}

func handlePackages(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(packageList)
}

func handleCaddyVersions(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(caddyVersions)
}

// corsMiddleware adds CORS headers to all responses
func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		// Handle preflight requests
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next(w, r)
	}
}

// registerHandlers registers all HTTP handlers with CORS middleware
func registerHandlers() {
	http.HandleFunc("/api/packages", corsMiddleware(handlePackages))
	http.HandleFunc("/api/download", corsMiddleware(handleDownload))
	http.HandleFunc("/api/caddy_versions", corsMiddleware(handleCaddyVersions))
	http.HandleFunc("/ws", corsMiddleware(handleWebSocket))
}
