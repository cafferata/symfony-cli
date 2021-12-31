package envs

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	docker "github.com/docker/docker/client"
	"github.com/symfony-cli/terminal"
)

var (
	dockerComposeNormalizeRegexp       = regexp.MustCompile("[^-_a-z0-9]")
	dockerComposeNormalizeRegexpLegacy = regexp.MustCompile("[^a-z0-9]")
)

type sortedPorts []types.Port

func (ps sortedPorts) Len() int           { return len(ps) }
func (ps sortedPorts) Swap(i, j int)      { ps[i], ps[j] = ps[j], ps[i] }
func (ps sortedPorts) Less(i, j int) bool { return ps[i].PrivatePort < ps[j].PrivatePort }

// Port of https://github.com/docker/compose/blob/615c01c50a51408a7fdfed66ecccf73781e87f2c/compose/cli/command.py#L153-L154
func normalizeDockerComposeProjectName(projectName string) string {
	return dockerComposeNormalizeRegexp.ReplaceAllString(strings.ToLower(projectName), "")
}

// Port of https://github.com/docker/compose/blob/4e0fdd70bdae4f8d85e4ef9d0129ce445f3ece3c/compose/cli/command.py#L129-L130
// (prior to 615c01c50a51408a7fdfed66ecccf73781e87f2c)
// This was used in Docker Compose prior to 1.21.0, some users might still use
// versions older though, so we keep this BC in the mean time.
func normalizeDockerComposeProjectNameLegacy(projectName string) string {
	return dockerComposeNormalizeRegexpLegacy.ReplaceAllString(strings.ToLower(projectName), "")
}

func (l *Local) RelationshipsFromDocker() Relationships {
	project := l.getComposeProjectName()
	if project == "" {
		return nil
	}

	opts := [](docker.Opt){docker.FromEnv}
	if host := os.Getenv("DOCKER_HOST"); host != "" && !strings.HasPrefix(host, "unix://") {
		// Setting a dialer on top of a unix socket breaks the connection
		// as the client then tries to connect to http:///path/to/socket and
		// thus tries to resolve the /path/to/socket host
		dialer := &net.Dialer{
			Timeout: 2 * time.Second,
		}
		opts = append(opts, docker.WithDialer(dialer))
	}
	client, err := docker.NewClientWithOpts(opts...)
	if err != nil {
		if l.Debug {
			fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
		}
		return nil
	}
	defer client.Close()

	client.NegotiateAPIVersion(context.Background())

	containers, err := client.ContainerList(context.Background(), types.ContainerListOptions{})
	if err != nil {
		if docker.IsErrConnectionFailed(err) {
			terminal.Logger.Warn().Msg(err.Error())
		} else if l.Debug {
			fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
		}
		return nil
	}

	// To be in sync with Docker compose behavior we also normalize project name
	// passed by the environment variable
	projectLegacy := normalizeDockerComposeProjectNameLegacy(project)
	project = normalizeDockerComposeProjectName(project)

	relationships := Relationships{}
	for _, container := range containers {
		p, ok := container.Labels["com.docker.compose.project"]
		if !ok {
			continue
		}
		if p != project && p != projectLegacy {
			continue
		}
		for suffix, relationship := range l.dockerServiceToRelationship(client, container) {
			// get the service name
			name, ok := container.Labels["com.symfony.server.service-prefix"]
			if !ok {
				name, ok = container.Labels["com.docker.compose.service"]
				if !ok {
					if l.Debug {
						fmt.Fprintf(os.Stderr, "ERROR: unable to find the service name of the Docker container with id \"%s\"\n", container.ID)
					}
					continue
				}
			}
			if l.Debug {
				fmt.Fprintf(os.Stderr, "  exposing service \"%s%s\"\n", name, suffix)
			}
			relationships[name+suffix] = append(relationships[name+suffix], relationship)
		}
	}

	if len(relationships) != 0 {
		l.DockerEnv = true
	}

	return relationships
}

func (l *Local) dockerServiceToRelationship(client *docker.Client, container types.Container) map[string]map[string]interface{} {
	if l.Debug {
		fmt.Fprintf(os.Stderr, `found Docker container "%s" for project "%s" (image "%s")`+"\n", container.Labels["com.docker.compose.service"], container.Labels["com.docker.compose.project"], container.Image)
	}

	if v, ok := container.Labels["com.symfony.server.service-ignore"]; ok && v == "True" {
		if l.Debug {
			fmt.Fprintln(os.Stderr, "  skipping as com.symfony.server.service-ignore is true")
		}
		return nil
	}

	var exposedPorts sortedPorts
	for _, port := range container.Ports {
		if port.PublicPort == 0 {
			continue
		}
		if l.Debug {
			fmt.Fprintf(os.Stderr, "  port %d for private port %d\n", port.PublicPort, port.PrivatePort)
		}
		exposedPorts = append(exposedPorts, port)
	}
	if len(exposedPorts) == 0 {
		if l.Debug && len(container.Ports) > 0 {
			fmt.Fprintf(os.Stderr, "WARNING: Container %s has none of its ports exposed.\n", container.Names[0][1:])
		}
		return nil
	}

	c, err := client.ContainerInspect(context.Background(), container.ID)
	if err != nil {
		if l.Debug {
			fmt.Fprintf(os.Stderr, "  ERROR: error while inspecting container \"%s\": %s\n", container.Names[0][1:], err)
		}
		return nil
	}

	if l.Debug {
		for _, env := range c.Config.Env {
			fmt.Fprintf(os.Stderr, "  env  %s\n", env)
		}
	}

	host := os.Getenv("DOCKER_HOST")
	if host == "" || strings.HasPrefix(host, "unix://") {
		host = "127.0.0.1"
	} else {
		u, err := url.Parse(host)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ERROR: unable to parse DOCKER_HOST \"%s\", falling back to 127.0.0.1: %s\n", host, err)
			host = "127.0.0.1"
		} else {
			host = u.Hostname()
		}
	}

	sort.Sort(exposedPorts)
	for _, p := range exposedPorts {
		rels := make(map[string]map[string]interface{})
		if p.PrivatePort == 1025 {
			// recommended image: schickling/mailcatcher
			for _, pw := range exposedPorts {
				if pw.PrivatePort == 1080 || pw.PrivatePort == 8025 {
					rels["-web"] = map[string]interface{}{
						"host":   host,
						"ip":     host,
						"port":   formatDockerPort(pw.PublicPort),
						"rel":    "mailer",
						"scheme": "http",
					}
					rels[""] = map[string]interface{}{
						"host":   host,
						"ip":     host,
						"port":   formatDockerPort(p.PublicPort),
						"rel":    "mailer",
						"scheme": "smtp",
					}
					return rels
				}
			}
		} else if p.PrivatePort == 25 {
			// recommended image: djfarrelly/maildev
			for _, pw := range exposedPorts {
				if pw.PrivatePort == 80 {
					rels["-web"] = map[string]interface{}{
						"host":   host,
						"ip":     host,
						"port":   formatDockerPort(pw.PublicPort),
						"rel":    "mailer",
						"scheme": "http",
					}
					rels[""] = map[string]interface{}{
						"host":   host,
						"ip":     host,
						"port":   formatDockerPort(p.PublicPort),
						"rel":    "mailer",
						"scheme": "smtp",
					}
					return rels
				}
			}
		} else if p.PrivatePort == 8707 {
			// Blackfire
			rels[""] = map[string]interface{}{
				"host":   host,
				"ip":     host,
				"port":   formatDockerPort(p.PublicPort),
				"rel":    "blackfire",
				"scheme": "tcp",
			}
			return rels
		} else if p.PrivatePort == 3306 {
			username := ""
			password := ""
			path := ""
			// MARIADB is used by bitnami/mariadb
			for _, prefix := range []string{"MYSQL", "MARIADB"} {
				for _, env := range c.Config.Env {
					if strings.HasPrefix(env, prefix+"_ROOT_PASSWORD") && password == "" {
						// *_PASSWORD has precedence over *_ROOT_PASSWORD
						password = getEnvValue(env, prefix+"_ROOT_PASSWORD")
						username = "root"
					} else if strings.HasPrefix(env, prefix+"_USER") {
						username = getEnvValue(env, prefix+"_USER")
					} else if strings.HasPrefix(env, prefix+"_PASSWORD") {
						password = getEnvValue(env, prefix+"_PASSWORD")
					} else if strings.HasPrefix(env, prefix+"_DATABASE") {
						path = getEnvValue(env, prefix+"_DATABASE")
					}
				}
			}
			if path == "" {
				path = username
			}
			rels[""] = map[string]interface{}{
				"host":     host,
				"ip":       host,
				"username": username,
				"password": password,
				"path":     path,
				"port":     formatDockerPort(p.PublicPort),
				"query": map[string]bool{
					"is_master": true,
				},
				"rel":    "mysql",
				"scheme": "mysql",
			}
			return rels
		} else if p.PrivatePort == 5432 {
			username := ""
			password := ""
			path := ""
			for _, env := range c.Config.Env {
				if strings.HasPrefix(env, "POSTGRES_USER") {
					username = getEnvValue(env, "POSTGRES_USER")
				} else if strings.HasPrefix(env, "POSTGRES_PASSWORD") {
					password = getEnvValue(env, "POSTGRES_PASSWORD")
				} else if strings.HasPrefix(env, "POSTGRES_DB") {
					path = getEnvValue(env, "POSTGRES_DB")
				}
			}
			if path == "" {
				path = username
			}
			rels[""] = map[string]interface{}{
				"host":     host,
				"ip":       host,
				"username": username,
				"password": password,
				"path":     path,
				"port":     formatDockerPort(p.PublicPort),
				"query": map[string]bool{
					"is_master": true,
				},
				"rel":    "pgsql",
				"scheme": "pgsql",
			}
			return rels
		} else if p.PrivatePort == 6379 {
			rels[""] = map[string]interface{}{
				"host":   host,
				"ip":     host,
				"port":   formatDockerPort(p.PublicPort),
				"rel":    "redis",
				"scheme": "redis",
			}
			return rels
		} else if p.PrivatePort == 11211 {
			rels[""] = map[string]interface{}{
				"host":   host,
				"ip":     host,
				"port":   formatDockerPort(p.PublicPort),
				"rel":    "memcached",
				"scheme": "memcached",
			}
			return rels
		} else if p.PrivatePort == 5672 {
			username := "guest"
			password := "guest"
			for _, env := range c.Config.Env {
				// that's our local convention
				if strings.HasPrefix(env, "RABBITMQ_DEFAULT_USER") {
					username = getEnvValue(env, "RABBITMQ_DEFAULT_USER")
				} else if strings.HasPrefix(env, "RABBITMQ_DEFAULT_PASS") {
					password = getEnvValue(env, "RABBITMQ_DEFAULT_PASS")
				}
			}
			rels[""] = map[string]interface{}{
				"host":     host,
				"ip":       host,
				"port":     formatDockerPort(p.PublicPort),
				"username": username,
				"password": password,
				"rel":      "amqp",
				"scheme":   "amqp",
			}
			// management plugin?
			for _, pw := range exposedPorts {
				if pw.PrivatePort == 15672 {
					rels["-management"] = map[string]interface{}{
						"host":   host,
						"ip":     host,
						"port":   formatDockerPort(pw.PublicPort),
						"rel":    "amqp",
						"scheme": "http",
					}
					break
				}
			}
			return rels
		} else if p.PrivatePort == 9200 {
			rels[""] = map[string]interface{}{
				"host":   host,
				"ip":     host,
				"port":   formatDockerPort(p.PublicPort),
				"rel":    "elasticsearch",
				"scheme": "http",
			}
			return rels
		} else if p.PrivatePort == 27017 {
			path := ""
			for _, env := range c.Config.Env {
				// that's our local convention
				if strings.HasPrefix(env, "MONGO_DATABASE") {
					path = getEnvValue(env, "MONGO_DATABASE")
				}
			}
			rels[""] = map[string]interface{}{
				"host":     host,
				"ip":       host,
				"username": "",
				"password": "",
				"path":     path,
				"port":     formatDockerPort(p.PublicPort),
				"rel":      "mongodb",
				"scheme":   "mongodb",
			}
			return rels
		} else if p.PrivatePort == 9092 {
			rels[""] = map[string]interface{}{
				"host":   host,
				"ip":     host,
				"port":   formatDockerPort(p.PublicPort),
				"rel":    "kafka",
				"scheme": "kafka",
			}
			return rels
		} else if p.PrivatePort == 80 && container.Image == "dunglas/mercure" {
			rels[""] = map[string]interface{}{
				"host":   host,
				"ip":     host,
				"port":   formatDockerPort(p.PublicPort),
				"rel":    "mercure",
				"scheme": "http",
			}
			return rels
		}

		if l.Debug {
			fmt.Fprintln(os.Stderr, "  exposing port")
		}

		rels[""] = map[string]interface{}{
			"host": host,
			"ip":   host,
			"port": formatDockerPort(p.PublicPort),
			"rel":  "simple",
		}
		return rels
	}

	return nil
}

func formatDockerPort(port uint16) string {
	return strconv.FormatInt(int64(port), 10)
}

func getEnvValue(env string, key string) string {
	if len(key) == len(env) {
		return ""
	}

	return env[len(key)+1:]
}

func (l *Local) getComposeProjectName() string {
	// https://docs.docker.com/compose/reference/envvars/#compose_project_name
	if project := os.Getenv("COMPOSE_PROJECT_NAME"); project != "" {
		return project
	}

	// COMPOSE_PROJECT_NAME can be set in a .env file
	if _, err := os.Stat(filepath.Join(l.Dir, ".env")); err == nil {
		if contents, err := ioutil.ReadFile(filepath.Join(l.Dir, ".env")); err == nil {
			for _, line := range bytes.Split(contents, []byte("\n")) {
				if bytes.HasPrefix(line, []byte("COMPOSE_PROJECT_NAME=")) {
					return string(line[len("COMPOSE_PROJECT_NAME="):])
				}
			}
		}
	}

	// https://docs.docker.com/compose/reference/envvars/#compose_file
	if os.Getenv("COMPOSE_FILE") != "" {
		return filepath.Base(l.Dir)
	}

	// look for the first dir up with a docker-composer.ya?ml file (in case of a multi-project)
	dir := l.Dir
	for {
		if _, err := os.Stat(filepath.Join(dir, "docker-compose.yaml")); err == nil {
			return filepath.Base(dir)
		}
		// both .yml and .yaml are supported by Docker composer
		if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err == nil {
			return filepath.Base(dir)
		}
		upDir := filepath.Dir(dir)
		if upDir == dir || upDir == "." {
			if l.Debug {
				fmt.Fprintln(os.Stderr, "ERROR: unable to find a docker-compose.yaml for the current directory")
			}
			return ""
		}
		dir = upDir
	}
}