package main

// This is the main entrypoint for the Prox Builder server
// The Prox Builder Server is a portable, optionally-distributed build
// service API for the Caddy webserver.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Config is the configuration for the Prox Builder server
type Config struct {
	// IsAgent is true if the server is running in agent mode
	IsAgent bool
	// IsHub is true if the server is running in hub mode
	IsHub bool
	// RedisHost is the host of the Redis server
	RedisHost string
	// RedisPort is the port of the Redis server
	RedisPort string
	// RedisUsername is the username for the Redis server
	RedisUsername string
	// RedisPassword is the password for the Redis server
	RedisPassword string
	// WebserverPort is the port of the web server
	WebserverPort string
	// SimultaneousJobs is the number of jobs that can be processed simultaneously
	SimultaneousJobs int
	// PackageEndpoint is the upstream endpoint for downloading the package list
	PackageEndpoint string
	// BinaryPath is the path to the directory where build artifacts are stored
	BinaryPath string
}

var (
	// The configuration for the server
	config Config
	// The Redis client
	rdb *redis.Client
	// A local copy of the package list downloaded from the upstream endpoint
	packageList json.RawMessage
	// A list of caddy versions that are available for download
	caddyVersions []string
	// The latest caddy commit SHA
	caddyCommitSHA string
)

func main() {
	// Load our configuration from command-line flags and environment variables
	config = parseConfig()
	// Initialize the running job count
	runningJobCount.Store(0)

	if config.IsAgent {
		log.Printf("Running in agent mode with support for %d simultaneous jobs", config.SimultaneousJobs)

		err := setupRedis()
		if err != nil {
			log.Fatalf("Error setting up Redis: %v", err)
		}

		// Start job processing loop
		ctx := context.Background()
		for {
			// If we don't have any capacity, then sleep and spin around again
			// until a job finishes.
			if config.SimultaneousJobs > 0 && runningJobCount.Load() >= int32(config.SimultaneousJobs) {
				time.Sleep(1 * time.Second)
				continue
			}
			log.Printf("Waiting for job from Redis")
			// Dequeue job from Redis
			result, err := rdb.BLPop(ctx, 0, "jobs").Result()
			if err != nil {
				log.Printf("Error dequeuing job: %v", err)
				continue
			}

			// Deserialize the job
			var job Job
			err = json.Unmarshal([]byte(result[1]), &job)
			if err != nil {
				log.Printf("Error deserializing job: %v", err)
				continue
			}
			log.Printf("--------------------------------")
			log.Printf("Processing job: Caddy version: %s - Packages: %v - OS: %s, Arch: %s", job.CaddyVersion, job.Packages, job.OS, job.Arch)
			log.Printf("--------------------------------")

			go processJobWithLogging(ctx, job)
		}
	} else {
		if config.IsHub {
			err := setupRedis()
			if err != nil {
				log.Fatalf("Error setting up Redis: %v", err)
			}
		}

		// Fetch the latest caddy commit SHA
		caddyCommitSHA, err := fetchLatestCaddyCommitSHA()
		if err != nil {
			log.Fatalf("Error fetching latest caddy commit SHA: %v", err)
		}
		log.Printf("Latest caddy commit SHA: %s", caddyCommitSHA)
		caddyVersions = append(caddyVersions, "latest")

		// Fetch the latest caddy releases
		caddyReleases, err := fetchCaddyReleases()
		if err != nil {
			log.Fatalf("Error fetching caddy releases: %v", err)
		}
		log.Printf("Latest caddy releases: %v", caddyReleases)
		caddyVersions = append(caddyVersions, caddyReleases...)
		// Start the process that updates the package list every hour
		go keepPackageListFresh()

		// Start HTTP server for local processing
		uiFilesystem, err := fs.Sub(embeddedUI, "ui/dist")
		if err != nil {
			log.Fatalf("Error creating UI filesystem: %v", err)
		}
		http.Handle("/", http.FileServer(http.FS(uiFilesystem)))
		http.HandleFunc("/api/packages", corsMiddleware(handlePackages))
		http.HandleFunc("/api/download", corsMiddleware(handleDownload))
		http.HandleFunc("/api/caddy_versions", corsMiddleware(handleCaddyVersions))
		http.HandleFunc("/ws", corsMiddleware(handleWebSocket))
		log.Printf("Starting server on :%s", config.WebserverPort)
		log.Fatal(http.ListenAndServe(":"+config.WebserverPort, nil))
	}
}

// parseConfig parses the command-line flags and environment variables
// and returns a Config struct.
// Environment variables take precedence over command-line flags.
func parseConfig() Config {
	config := Config{}

	// Parse command line flags
	flag.BoolVar(&config.IsAgent, "agent", false, "Run in agent mode")
	flag.BoolVar(&config.IsHub, "hub", false, "Run in hub mode (dispatching jobs to agents)")
	flag.StringVar(&config.RedisHost, "redis-host", "localhost", "Redis host")
	flag.StringVar(&config.RedisPort, "redis-port", "6379", "Redis port")
	flag.StringVar(&config.RedisUsername, "redis-username", "", "Redis username")
	flag.StringVar(&config.RedisPassword, "redis-password", "", "Redis password")
	flag.StringVar(&config.WebserverPort, "webserver-port", "8080", "Webserver port")
	flag.IntVar(&config.SimultaneousJobs, "simultaneous-jobs", 2, "Number of simultaneous jobs that can be processed (if running in agent mode)")
	flag.StringVar(&config.PackageEndpoint, "package-endpoint", "https://caddyserver.com/api/packages", "Upstream endpoint for downloading the package list")
	flag.StringVar(&config.BinaryPath, "binary-path", "data", "Path to the directory where build artifacts are stored")
	flag.Parse()

	// Check environment variables
	if os.Getenv("PB_AGENT") != "" {
		config.IsAgent = true
	}
	if os.Getenv("PB_HUB") != "" {
		config.IsHub = true
	}
	if host := os.Getenv("PB_REDIS_HOST"); host != "" {
		config.RedisHost = host
	}
	if port := os.Getenv("PB_REDIS_PORT"); port != "" {
		config.RedisPort = port
	}
	if username := os.Getenv("PB_REDIS_USERNAME"); username != "" {
		config.RedisUsername = username
	}
	if password := os.Getenv("PB_REDIS_PASSWORD"); password != "" {
		config.RedisPassword = password
	}
	if webserverPort := os.Getenv("PB_WEBSERVER_PORT"); webserverPort != "" {
		config.WebserverPort = webserverPort
	}
	if simultaneousJobs := os.Getenv("PB_SIMULTANEOUS_JOBS"); simultaneousJobs != "" {
		simultaneousJobsInt, err := strconv.Atoi(simultaneousJobs)
		if err != nil {
			log.Fatalf("Error parsing simultaneous-jobs: %v", err)
		}
		config.SimultaneousJobs = simultaneousJobsInt
	}
	if packageEndpoint := os.Getenv("PB_PACKAGE_ENDPOINT"); packageEndpoint != "" {
		config.PackageEndpoint = packageEndpoint
	}
	if binaryPath := os.Getenv("PB_BINARY_PATH"); binaryPath != "" {
		config.BinaryPath = binaryPath
	}
	if config.IsAgent && config.IsHub {
		log.Fatalf("Cannot run in both agent and hub mode")
	}

	return config
}

// setupRedis creates a new Redis client and connects to the Redis server.
// It returns an error if the connection fails.
func setupRedis() error {
	// Start Redis client
	rdb = redis.NewClient(&redis.Options{
		Addr:     config.RedisHost + ":" + config.RedisPort,
		Username: config.RedisUsername,
		Password: config.RedisPassword,
	})
	log.Printf("Connecting to Redis at %s:%s", config.RedisHost, config.RedisPort)
	_, err := rdb.Ping(context.Background()).Result()
	if err != nil {
		return err
	}
	log.Printf("Connected to Redis")
	return nil
}

func downloadPackageList() error {
	resp, err := http.Get(config.PackageEndpoint)
	if err != nil {
		return fmt.Errorf("error downloading package list: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("error reading package list: %v", err)
	}
	packageList = body
	return nil
}

// This function downloads the package list once per hour or until the
// process exits.
func keepPackageListFresh() {
	for {
		log.Printf("Updating package list")
		err := downloadPackageList()
		if err != nil {
			log.Printf("Error downloading package list: %v", err)
			time.Sleep(5 * time.Minute)
			continue
		}
		time.Sleep(1 * time.Hour)
	}
}

// Fetch the latest git commit SHA for the upstream caddy repository
func fetchLatestCaddyCommitSHA() (string, error) {
	resp, err := http.Get("https://api.github.com/repos/caddyserver/caddy/commits")
	if err != nil {
		return "", fmt.Errorf("error fetching latest caddy commit SHA: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading caddy commit list: %v", err)
	}

	var commits []map[string]interface{}
	err = json.Unmarshal(body, &commits)
	if err != nil {
		return "", fmt.Errorf("error unmarshalling caddy commit list: %v", err)
	}

	commitSHA := commits[0]["sha"].(string)
	// Trim the commit SHA to the first 7 characters
	commitSHA = commitSHA[:7]

	return commitSHA, nil
}

func fetchCaddyReleases() ([]string, error) {
	resp, err := http.Get("https://api.github.com/repos/caddyserver/caddy/releases")
	if err != nil {
		return nil, fmt.Errorf("error fetching caddy releases: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading caddy release list: %v", err)
	}

	var releases []map[string]interface{}
	err = json.Unmarshal(body, &releases)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling caddy release list: %v", err)
	}

	versions := make([]string, len(releases))
	for i, release := range releases {
		versions[i] = release["tag_name"].(string)
	}

	return versions, nil
}
