## Prox Builder - A distributed build server for Caddy binaries

Prox Builder is a build server for Caddy binaries. Currently, Caddy hosts an online build system at https://caddyserver.com/download, however it is a massive piece of infrastructure for them to host freely for everyone wanting custom binary builds.

As a result, I wrote this API-compatible build server that can optionally be distributed horizontally to accommodate a number of custom binary build requests. It can also optionally cache builds to local storage to speed up binary generation times.

![Screenshot of the package selection screen](/.github/images/build-demo-step1.png)

![Screenshot of the build process log output](/.github/images/build-demo-step2.png)

### Features

- Cache builds locally to quickly service duplicate build requests (Great for CI)
- Delegate builds to multiple agents to maintain throughput in times of high demand
- Stream logs back to the client in realtime - If your build fails due to a broken module, you can quickly pinpoint why
- Select from a number of Caddy base versions - If you're locked to a certain version of Caddy, easily select it as the baseline of your build
- Embedded web UI

### Quick Start

#### Docker

```bash
docker run --rm -it -p 8080:8080 proxbuilder:latest
```

#### Docker (docker-compose with a hub, agent and shared redis queue)

```bash
docker-compose up
```

#### Binaries

TODO

### Operating Modes

Prox Builder can operate in a number of modes depending on your scalability and performance needs.

| Mode       | Command-line Flag | Description                                                                                                                                                                                 |
| ---------- | ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Standalone | (blank)           | When neither `--hub` nor `--agent` are specified, run the webserver and coordinate the xcaddy builds all in the same process.                                                               |
| Hub        | `--hub`           | Run the webserver on this process and dispatch jobs to a redis queue for a number of agents to work in the background. Requires Redis to communicate with build agents.                     |
| Agent      | `--agent`         | This process will only execute xcaddy builds and copy them to the given storage location after they're done. Requires Redis to receive build jobs from the hub and relay log messages back. |

### Configuration

Prox Builder can be configured either by specifying command-line flags or by setting environment variables. Environment variables will always take precedence.
| Command-line Flag | Environment Variable | Description |
| ----------------- | -------------------- | ------------------------------------------------------------------------------------------------------------------- |
| `--agent` | `PB_AGENT` | This process will only execute xcaddy builds and copy them to the given storage location after they're done. |
| `--hub` | `PB_HUB` | Run the webserver on this process and dispatch jobs to a redis queue for a number of agents to work in the background. |
| `--redis-host` | `PB_REDIS_HOST` | The host to use when connecting to the redis server in hub or agent mode. (Default: localhost) |
| `--redis-port` | `PB_REDIS_PORT` | The port of the redis server in hub or agent mode. (Default: 6379) |
| `--redis-username` | `PB_REDIS_USERNAME` | The username to use when logging into the redis server in hub or agent mode. (Default: blank) |
| `--redis-password` | `PB_REDIS_PASSWORD` | The password to use when logging into the redis server in hub or agent mode. (Default: blank) |
| `--simultaneous-jobs` | `PB_SIMULTANEOUS_JOBS` | The number of concurrent build jobs to run when in agent mode (Default: 2) |
| `--webserver-port` | `PB_WEBSERVER_PORT` | The port to listen on when running in hub or standalone mode (Default: 8080) |
| `--package-endpoint` | `PB_PACKAGE_ENDPOINT` | The endpoint to download the list of caddy modules/packages from (Default: caddyserver.com/api/packages) |
| `--binary-path` | `PB_BINARY_PATH` | The path where newly-built Caddy binaries are stored |

### API Endpoints

`GET /api/download` - Build and download a new Caddy server binary

URL parameters are as follows:

| Parameter       | Description                                                                                                               |
| --------------- | ------------------------------------------------------------------------------------------------------------------------- |
| `os`            | The operating system to build for (e.g., linux, darwin, windows)                                                          |
| `arch`          | The CPU architecture to build for (e.g., amd64, arm64)                                                                    |
| `p[]`           | Array of plugin packages to include in the build (e.g., github.com/caddy-dns/cloudflare). Repeat as many times as needed. |
| `caddy_version` | The Caddy version to compile against                                                                                      |

### Things to be done

- Record a local copy of the download numbers rather than simply regurgitating what is in the upstream's /api/packages response payload
- Finish implementing S3 remote storage
- UI fixes for better responsiveness
- Tests
