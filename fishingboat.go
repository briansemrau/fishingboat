package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

type Resources struct {
	MilliCPU    int `json:"mcpu"`
	MemoryMi    int `json:"memoryMi"`
	GpuMemoryMi int `json:"gpuMemoryMi"`
}

type PortMapping struct {
	ContainerPort int   `json:"containerPort"`
	HostPorts     []int `json:"hostPorts"`
}

const (
	None         = ""
	Always       = "always"
	Never        = "never"
	IfNotPresent = "ifnotpresent"
)

type Service struct {
	Name            string        `json:"name"`
	ResourceRequest *Resources    `json:"resources,omitempty"`
	CoolDown        int           `json:"cooldown"`
	Ports           []PortMapping `json:"ports"`

	Image      string `json:"image"`
	PullPolicy string `json:"pullPolicy,omitempty"`
	HostIP     string `json:"hostIP,omitempty"`

	Cmd        []string              `json:"cmd,omitempty"`
	Config     *container.Config     `json:"config,omitempty"`
	HostConfig *container.HostConfig `json:"hostConfig,omitempty"`
}

type ServerResourceLimits struct {
	Limits Resources `json:"allocationLimits"`
}

type ServicesConfig struct {
	ProxyIP       string               `json:"proxyIP"`
	ServiceHostIP string               `json:"serviceHostIP"`
	Resources     ServerResourceLimits `json:"resources"`
	Services      []Service            `json:"services"`
}

type Server struct {
	Config ServicesConfig

	ServerLock sync.RWMutex

	ServiceProxyHostPortMap map[string]map[int]int
	ServiceConnCount        map[string]uint
	ServiceKillTime         map[string]time.Time

	TrackedResourcesLock sync.RWMutex
	TrackedResources     Resources

	// prevent concurrent docker api calls per container
	ContainerAPILock *MutexMap
}

func (s *Server) Start() (err error) {
	// Listen on all configured ports
	for _, app := range s.Config.Services {
		for _, port := range app.Ports {
			for _, hostPort := range port.HostPorts {
				listener, err := net.Listen("tcp", s.Config.ProxyIP+":"+fmt.Sprint(hostPort))
				if err != nil {
					log.Println("Error listening on port", hostPort, "for application", app.Name, ":", err.Error())
					return err
				}
				defer listener.Close()
				log.Println("Listening on port", hostPort, "for application", app.Name)
				go s.Listen(listener, app, port)
			}
		}
	}
	// blocking
	s.CleanUpContainers()
	return
}

func (s *Server) CleanUpContainers() {
	for {
		toKill := make([]string, 0)
		func() {
			s.ServerLock.RLock()
			defer s.ServerLock.RUnlock()
			for container, ts := range s.ServiceKillTime {
				if time.Since(ts).Seconds() > 0 {
					if count, ok := s.ServiceConnCount[container]; ok {
						if count == 0 {
							toKill = append(toKill, container)
						}
					} else {
						log.Println("Container", container, "was scheduled to die, but connection ref count is nil.")
					}
				}
			}
		}()
		for _, container := range toKill {
			log.Println("Stopping container", container)
			err := s.StopContainer(container)
			if err != nil {
				log.Println("Error stopping container", container, ":", err.Error())
			}
			func() {
				s.ServerLock.Lock()
				defer s.ServerLock.Unlock()
				delete(s.ServiceKillTime, container)
			}()
		}
		time.Sleep(1 * time.Second)
	}
}

func (s *Server) FindOpenPort(ip string) (int, error) {
	rangeStart := 49152
	rangeEnd := 65535

	attemptPort := rangeStart + rand.Intn(rangeEnd-rangeStart)
	maxAttempts := 32
	for i := 0; i < maxAttempts; i++ {
		listener, err := net.Listen("tcp", ip+":"+fmt.Sprint(attemptPort))
		if err != nil {
			// port is in use, try next port
			attemptPort = (attemptPort + 1)
			if attemptPort > rangeEnd {
				attemptPort = rangeStart
			}
		} else {
			listener.Close()
			return attemptPort, nil
		}
	}
	return -1, fmt.Errorf("could not find open port after %d attempts", maxAttempts)
}

func (s *Server) Listen(listener net.Listener, app Service, port PortMapping) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Println("Error accepting connection: ", err.Error())
			continue
		}
		log.Println("Accepted connection for application", app.Name, "on port", port.ContainerPort, "from", conn.RemoteAddr())
		go s.HandleConnection(conn, app, port)
	}
}

func (s *Server) HandleConnection(src net.Conn, app Service, port PortMapping) {
	defer src.Close()

	containerActive := false
	func() {
		s.ServerLock.RLock()
		defer s.ServerLock.RUnlock()
		if count, ok := s.ServiceConnCount[app.Name]; ok {
			containerActive = count > 0
		}
	}()
	if !containerActive {
		err := s.LaunchContainer(app)
		if err != nil {
			log.Println("Error launching container: ", err.Error())
			return
		}
	}

	// refcount
	func() {
		s.ServerLock.Lock()
		defer s.ServerLock.Unlock()
		s.ServiceConnCount[app.Name]++
	}()
	// on closed, give the container a deadline
	defer func() {
		s.ServerLock.Lock()
		defer s.ServerLock.Unlock()
		s.ServiceConnCount[app.Name]--
		if count, ok := s.ServiceConnCount[app.Name]; ok {
			if count == 0 {
				s.ServiceKillTime[app.Name] = time.Now().Add(time.Duration(app.CoolDown) * time.Second)
			}
		}
	}()

	// connect to container
	hostIP := s.Config.ServiceHostIP
	if app.HostIP != "" {
		hostIP = app.HostIP
	}
	var backendHostPort int
	func() {
		backendHostPort = -1
		s.ServerLock.RLock()
		defer s.ServerLock.RUnlock()
		if m, ok := s.ServiceProxyHostPortMap[app.Name]; ok {
			if p, ok := m[port.ContainerPort]; ok {
				backendHostPort = p
			}
		}
	}()
	dest, err := net.DialTimeout("tcp", hostIP+":"+fmt.Sprint(backendHostPort), 10*time.Second)
	if err != nil {
		log.Println("Error connecting to destination: ", err.Error())
		return
	}
	defer dest.Close()

	waitGroup := sync.WaitGroup{}
	waitGroup.Add(2)
	copy := func(s io.Reader, d io.Writer) {
		_, err = io.Copy(d, s)
		if err != nil {
			log.Println("Error copying from source to destination: ", err.Error())
		}
		waitGroup.Done()
	}
	go copy(src, dest)
	go copy(dest, src)
	waitGroup.Wait()
	log.Println("Closed connection for application", app.Name, "on port", port.ContainerPort, "from", src.RemoteAddr())
}

func (s *Server) LaunchContainer(app Service) (err error) {
	s.ContainerAPILock.Lock(app.Name)
	defer s.ContainerAPILock.Unlock(app.Name)

	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		panic(err)
	}
	defer cli.Close()

	containerName := app.Name + "-goscalezero"

	// Check if the container exists
	var cont *types.Container

	var list []types.Container
	list, err = cli.ContainerList(context.Background(), types.ContainerListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.KeyValuePair{Key: "name", Value: "/" + containerName}),
	})
	if err != nil {
		log.Println("Error listing containers: ", err.Error())
		return
	}
searchlist:
	for _, listcont := range list { // this seems expensive
		for _, name := range listcont.Names {
			if name == "/"+containerName {
				cont = &listcont
				break searchlist
			}
		}
	}

	// Check if container is valid
	if cont != nil && cont.Image != app.Image {
		log.Println("Container image does not match")

		// Remove the container
		err = cli.ContainerRemove(context.Background(), cont.ID, types.ContainerRemoveOptions{Force: true})
		if err != nil {
			log.Println("Error removing container: ", err.Error())
			return
		}

		cont = nil
	}

	var contID string

	// TODO this control flow is messy. should be redesigned/refactored
	defer func() {
		if err != nil {
			return
		}
		// Store port mappings (if not already stored)
		needPortMappings := false
		func() {
			s.ServerLock.RLock()
			defer s.ServerLock.RUnlock()
			if _, ok := s.ServiceProxyHostPortMap[app.Name]; !ok {
				needPortMappings = true
			}
		}()
		if needPortMappings {
			err = func() (err error) {
				var inspect types.ContainerJSON
				inspect, err = cli.ContainerInspect(context.Background(), contID)
				if err != nil {
					log.Println("Error inspecting container: ", err.Error())
					return
				}

				s.ServerLock.Lock()
				defer s.ServerLock.Unlock()

				if _, ok := s.ServiceProxyHostPortMap[app.Name]; !ok {
					s.ServiceProxyHostPortMap[app.Name] = make(map[int]int)
				}
				for natport, bindings := range inspect.HostConfig.PortBindings {
					var containerPort int
					containerPort, err = strconv.Atoi(strings.Split(string(natport), "/")[0])
					if err != nil {
						log.Println("Error parsing port: ", err.Error())
						return
					}
					var backendHostPort int
					backendHostPort, err = strconv.Atoi(bindings[0].HostPort)
					if err != nil {
						log.Println("Error parsing port: ", err.Error())
						return
					}
					s.ServiceProxyHostPortMap[app.Name][containerPort] = backendHostPort
				}
				return
			}()
			if err != nil {
				return
			}
		}
	}()

	if cont == nil {
		log.Println("Container does not exist")

		// Pull the image
		switch strings.ToLower(app.PullPolicy) {
		case Always:
			log.Println("Pulling image with pull policy Always. This is not recommended. Consider using IfNotPresent.")
			func() {
				var resp io.ReadCloser
				resp, err = cli.ImagePull(context.Background(), app.Image, types.ImagePullOptions{})
				if err != nil {
					log.Println("Error pulling image: ", err.Error())
					return // continue with old image
				}
				io.Copy(os.Stdout, resp)
			}()
		case IfNotPresent:
			// check if image exists
			func() {
				var images []types.ImageSummary
				images, err = cli.ImageList(context.Background(), types.ImageListOptions{})
				if err != nil {
					log.Println("Error listing images: ", err.Error())
					return // continue with old image
				}
				for _, image := range images {
					for _, tag := range image.RepoTags {
						if tag == app.Image {
							log.Println("Existing image found for", app.Image)
							return // continue with old image
						}
					}
				}
				var resp io.ReadCloser
				resp, err = cli.ImagePull(context.Background(), app.Image, types.ImagePullOptions{})
				if err != nil {
					log.Println("Error pulling image: ", err.Error())
					return // will fail because no image
				}
				io.Copy(os.Stdout, resp)
			}()
		case Never, None: // do nothing
		default:
			log.Println("Unknown pull policy: ", app.PullPolicy)
		}

		// Create the container
		hostIP := s.Config.ServiceHostIP
		if app.HostIP != "" {
			hostIP = app.HostIP
		}
		portMap := nat.PortMap{}
		for _, port := range app.Ports {
			var containerPort nat.Port
			containerPort, err = nat.NewPort("tcp", fmt.Sprint(port.ContainerPort))
			if err != nil {
				log.Println("Port not available: ", err.Error())
				return
			}
			portBindings := make([]nat.PortBinding, 1)

			// Find open host ports to bind the container to
			var backendHostPort int
			err = func() (err error) {
				s.ServerLock.Lock()
				defer s.ServerLock.Unlock()

				if _, ok := s.ServiceProxyHostPortMap[app.Name]; !ok {
					s.ServiceProxyHostPortMap[app.Name] = make(map[int]int)
				}
				s.ServiceProxyHostPortMap[app.Name][port.ContainerPort], err = s.FindOpenPort(hostIP)
				if err != nil {
					log.Println("Error finding open port: ", err.Error())
					return
				}
				log.Println("Found open port", s.ServiceProxyHostPortMap[app.Name][port.ContainerPort], "for application", app.Name, "on host", hostIP, "for container port", port.ContainerPort, "on proxy", s.Config.ProxyIP)
				backendHostPort = s.ServiceProxyHostPortMap[app.Name][port.ContainerPort]
				return
			}()
			if err != nil {
				return
			}

			portBindings[0] = nat.PortBinding{
				HostIP:   hostIP,
				HostPort: fmt.Sprint(backendHostPort),
			}
			portMap[containerPort] = portBindings
		}

		resources := container.Resources{}
		if app.ResourceRequest.MemoryMi > 0 {
			resources.Memory = int64(app.ResourceRequest.MemoryMi * 1024 * 1024)
		}
		if app.ResourceRequest.MilliCPU > 0 {
			resources.NanoCPUs = int64(app.ResourceRequest.MilliCPU * 1000000)
		}
		if app.ResourceRequest.GpuMemoryMi > 0 {
			resources.DeviceRequests = []container.DeviceRequest{
				{
					Driver: "",
					Count:  -1,
					Capabilities: [][]string{
						{"gpu"},
					},
				},
			}
		}
		oomKillDisable := true
		resources.OomKillDisable = &oomKillDisable

		var config container.Config
		if app.Config != nil {
			configCopy := *app.Config
			config = configCopy
		} else {
			config = container.Config{}
		}
		config.Image = app.Image
		config.Cmd = app.Cmd

		var hostConfig container.HostConfig
		if app.HostConfig != nil {
			hostConfigCopy := *app.HostConfig
			hostConfig = hostConfigCopy
		} else {
			hostConfig = container.HostConfig{}
		}
		hostConfig.NetworkMode = container.NetworkMode("default")
		hostConfig.PortBindings = portMap
		hostConfig.Resources = resources

		var resp container.CreateResponse
		resp, err = cli.ContainerCreate(
			context.Background(),
			&config,
			&hostConfig,
			nil,
			nil,
			containerName,
		)
		if err != nil {
			log.Println("Error creating container: ", err.Error())
			return
		}
		contID = resp.ID
	} else {
		contID = cont.ID
		// Check if the container is already running
		if cont.State == "running" {
			log.Println("Container", cont.ID, "is already running")
			return
		} else {
			log.Println("Container is not running (state:" + cont.State + ")")
		}
	}

	err = func() error {
		s.TrackedResourcesLock.RLock()
		defer s.TrackedResourcesLock.RUnlock()
		if s.TrackedResources.MilliCPU+app.ResourceRequest.MilliCPU > s.Config.Resources.Limits.MilliCPU {
			return fmt.Errorf("not enough cpu resources to launch container")
		}
		if s.TrackedResources.MemoryMi+app.ResourceRequest.MemoryMi > s.Config.Resources.Limits.MemoryMi {
			return fmt.Errorf("not enough memory resources to launch container")
		}
		if s.TrackedResources.GpuMemoryMi+app.ResourceRequest.GpuMemoryMi > s.Config.Resources.Limits.GpuMemoryMi {
			return fmt.Errorf("not enough video memory resources to launch container")
		}
		return nil
	}()
	if err != nil {
		return
	}

	func() {
		// reserve resources
		s.TrackedResourcesLock.Lock()
		defer s.TrackedResourcesLock.Unlock()
		s.TrackedResources.MilliCPU += app.ResourceRequest.MilliCPU
		s.TrackedResources.MemoryMi += app.ResourceRequest.MemoryMi
		s.TrackedResources.GpuMemoryMi += app.ResourceRequest.GpuMemoryMi
	}()
	defer func() {
		if err != nil {
			// release unused resources
			s.TrackedResourcesLock.Lock()
			defer s.TrackedResourcesLock.Unlock()
			s.TrackedResources.MilliCPU -= app.ResourceRequest.MilliCPU
			s.TrackedResources.MemoryMi -= app.ResourceRequest.MemoryMi
			s.TrackedResources.GpuMemoryMi -= app.ResourceRequest.GpuMemoryMi
		}
	}()

	// Start the container
	err = cli.ContainerStart(context.Background(), contID, types.ContainerStartOptions{})
	if err != nil {
		log.Println("Error starting container: ", err.Error())
		return
	}

	// Wait for the container to start
	err = func() error {
		checkFreq := 100 * time.Millisecond
		checkTimeout := 10 * time.Second
		for i := 0; i < int(checkTimeout/checkFreq); i++ {
			cont, err := cli.ContainerInspect(context.Background(), contID)
			if err != nil {
				log.Println("Error inspecting container: ", err.Error())
				return err
			}
			if cont.State.Status != "running" {
				return fmt.Errorf("container is not running")
			}
			health := types.NoHealthcheck
			if cont.State.Health != nil {
				health = cont.State.Health.Status
			}
			if health == types.NoHealthcheck {
				if cont.State.Running {
					log.Println("", app.Name, "container is reported running after", i*int(checkFreq/time.Millisecond), "ms")
					return nil
				}
			} else if health == types.Healthy {
				log.Println("", app.Name, "container is reported healthy after", i*int(checkFreq/time.Millisecond), "ms")
				return nil
			}
			time.Sleep(checkFreq)
		}
		return fmt.Errorf("container did not start in time")
	}()
	if err != nil {
		return
	}

	log.Println("Started container", contID, "for application", app.Name)
	return
}

func (s *Server) StopContainer(name string) (err error) {
	s.ContainerAPILock.Lock(name)
	defer s.ContainerAPILock.Unlock(name)

	err = func() error {
		s.ServerLock.Lock()
		defer s.ServerLock.Unlock()
		if count, ok := s.ServiceConnCount[name]; ok {
			if count > 0 {
				log.Println("Container", name, "has active connections, not stopping")
				return fmt.Errorf("container has active connections")
			}
		}
		return nil
	}()
	if err != nil {
		return
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return
	}
	defer cli.Close()

	containerName := name + "-goscalezero"

	// Check if the container exists
	var cont *types.Container

	var list []types.Container
	list, err = cli.ContainerList(context.Background(), types.ContainerListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.KeyValuePair{Key: "name", Value: "/" + containerName}),
	})
	if err != nil {
		return
	}
searchlist:
	for _, listcont := range list {
		for _, name := range listcont.Names {
			if name == "/"+containerName {
				cont = &listcont
				break searchlist
			}
		}
	}

	// Check if container is valid
	if cont == nil {
		err = fmt.Errorf("container does not exist")
		return
	}

	// Stop command
	err = cli.ContainerStop(context.Background(), cont.ID, container.StopOptions{})
	if err != nil {
		return
	}

	// Wait for the container to stop
	ctxWithTimeout, cancelTimeout := context.WithTimeout(context.Background(), time.Second*10)
	defer cancelTimeout()
	chWaitResp, chErr := cli.ContainerWait(ctxWithTimeout, cont.ID, container.WaitConditionNotRunning)
	select {
	case err = <-chErr:
		if err != nil {
			return
		}
	case <-chWaitResp:
	}

	log.Println("Stopped container", cont.ID, "for application", name)

	func() {
		var service *Service
		for _, serv := range s.Config.Services {
			if serv.Name == name {
				service = &serv
				break
			}
		}
		if service == nil {
			log.Println("Could not find service config for", name)
			return
		}
		s.TrackedResourcesLock.Lock()
		defer s.TrackedResourcesLock.Unlock()
		s.TrackedResources.MilliCPU -= service.ResourceRequest.MilliCPU
		s.TrackedResources.MemoryMi -= service.ResourceRequest.MemoryMi
		s.TrackedResources.GpuMemoryMi -= service.ResourceRequest.GpuMemoryMi
	}()

	return
}

func (s *Server) ComposeUp() (err error) {
	// TODO support docker compose
	return
}

func (s *Server) ComposeDown() (err error) {
	// TODO support docker compose
	return
}

func (s *Server) ProcessStart() (err error) {
	// TODO support executable
	return
}

func (s *Server) ProcessTerminate() (err error) {
	// TODO support executables
	return
}

func main() {
	configBuf, err := os.ReadFile("services.json")
	if err != nil {
		panic(err)
	}
	config := new(ServicesConfig)
	if err = json.Unmarshal([]byte(configBuf), config); err != nil {
		panic(err)
	}

	server := &Server{
		Config:                  *config,
		ServerLock:              sync.RWMutex{},
		ServiceConnCount:        make(map[string]uint),
		ServiceKillTime:         make(map[string]time.Time),
		ServiceProxyHostPortMap: make(map[string]map[int]int),
		TrackedResourcesLock:    sync.RWMutex{},
		TrackedResources:        Resources{},
		ContainerAPILock:        NewMutexMap(),
	}
	err = server.Start()
	if err != nil {
		log.Println("Error starting server: ", err.Error())
		panic(err)
	}
}
