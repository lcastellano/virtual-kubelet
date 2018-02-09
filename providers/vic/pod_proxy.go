package vic

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/apex/log"
	units "github.com/docker/go-units"
	engerr "github.com/vmware/vic/lib/apiservers/engine/errors"
	"github.com/vmware/vic/lib/apiservers/portlayer/client"
	"github.com/vmware/vic/pkg/trace"

	"github.com/docker/docker/api/types/container"
	"github.com/moby/moby/api/types"
	"k8s.io/api/core/v1"
)

type VicPodProxy interface {
	CreatePod(ctx context.Context, pod *v1.Pod) error
}

type PodProxy struct {
	client     *client.PortLayer
	imageStore VicImageStore
	//containerStore
}

const (
	defaultEnvPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	// MemoryAlignMB is the value to which container VM memory must align in order for hotadd to work
	MemoryAlignMB = 128
	// MemoryMinMB - the minimum allowable container memory size
	MemoryMinMB = 512
	// MemoryDefaultMB - the default container VM memory size
	MemoryDefaultMB = 2048
	// MinCPUs - the minimum number of allowable CPUs the container can use
	MinCPUs = 1
	// DefaultCPUs - the default number of container VM CPUs
	DefaultCPUs = 2
)

func NewPodProxy(plClient *client.PortLayer, imageStore VicImageStore) VicPodProxy {
	if plClient == nil {
		return nil
	}

	return &PodProxy{
		client:     plClient,
		imageStore: imageStore,
	}
}

func (p *PodProxy) CreatePod(ctx context.Context, pod *v1.Pod) error {
	op := trace.FromContext(ctx, "createContainer")

	// Create each container.  Only for prototype only.
	for _, c := range pod.Spec.Containers {
		// Transform kube container config to docker create config
		createConfig := KubeSpecToDockerCreateSpec(c)

		err := p.createContainer(ctx, createConfig)
		if err != nil {
			op.Errorf("Failed to create container %s for pod %s", createConfig.Name, pod.Name)
		}
	}

	return nil
}

func (p *PodProxy) createContainer(ctx context.Context, config types.ContainerCreateConfig) error {
	op := trace.FromContext(ctx, "createContainer")

	// Pull image config from VIC's image store
	image, err := p.imageStore.Get(config.Config.Image)
	if err != nil {
		err = fmt.Errorf("PodProxy failed to get image %s's config from the image store: %s", err.Error())
		op.Error(err)
		return err
	}

	setCreateConfigOptions(config.Config, image.Config)
	op.Infof("config = %#v", config.Config)

	return nil
}

//------------------------------------
// Utility Functions
//------------------------------------

// TODO: refactor so we no longer need to know about docker types
func KubeSpecToDockerCreateSpec(cSpec v1.Container) types.ContainerCreateConfig {
	config := types.ContainerCreateConfig{
		Name: cSpec.Name,
		Config: &container.Config{
			WorkingDir: cSpec.WorkingDir,
			Image:      cSpec.Image,
			Tty:        cSpec.TTY,
			StdinOnce:  cSpec.StdinOnce,
			OpenStdin:  cSpec.Stdin,
		},
		HostConfig: &container.HostConfig{
		//container.Resources.CPUCount:
		},
	}

	if len(cSpec.Command) != 0 {
		config.Config.Cmd = cSpec.Command
	}

	//TODO:  Handle kube container's args (cSpec.Args)

	config.HostConfig.Resources.CPUCount = cSpec.Resources.Limits.Cpu().Value()
	config.HostConfig.Resources.Memory = cSpec.Resources.Limits.Memory().Value()

	return config
}

// SetConfigOptions is a place to add necessary container configuration
// values that were not explicitly supplied by the user
func setCreateConfigOptions(config, imageConfig *container.Config) {
	// Overwrite or append the image's config from the CLI with the metadata from the image's
	// layer metadata where appropriate
	if len(config.Cmd) == 0 {
		config.Cmd = imageConfig.Cmd
	}
	if config.WorkingDir == "" {
		config.WorkingDir = imageConfig.WorkingDir
	}
	if len(config.Entrypoint) == 0 {
		config.Entrypoint = imageConfig.Entrypoint
	}

	if config.Volumes == nil {
		config.Volumes = imageConfig.Volumes
	} else {
		for k, v := range imageConfig.Volumes {
			//NOTE: the value of the map is an empty struct.
			//      we also do not care about duplicates.
			//      This Volumes map is really a Set.
			config.Volumes[k] = v
		}
	}

	if config.User == "" {
		config.User = imageConfig.User
	}
	// set up environment
	config.Env = setEnvFromImageConfig(config.Tty, config.Env, imageConfig.Env)
}

func setEnvFromImageConfig(tty bool, env []string, imgEnv []string) []string {
	// Set PATH in ENV if needed
	env = setPathFromImageConfig(env, imgEnv)

	containerEnv := make(map[string]string, len(env))
	for _, e := range env {
		kv := strings.SplitN(e, "=", 2)
		var val string
		if len(kv) == 2 {
			val = kv[1]
		}
		containerEnv[kv[0]] = val
	}

	// Set TERM to xterm if tty is set, unless user supplied a different TERM
	if tty {
		if _, ok := containerEnv["TERM"]; !ok {
			env = append(env, "TERM=xterm")
		}
	}

	// add remaining environment variables from the image config to the container
	// config, taking care not to overwrite anything
	for _, imageEnv := range imgEnv {
		key := strings.SplitN(imageEnv, "=", 2)[0]
		// is environment variable already set in container config?
		if _, ok := containerEnv[key]; !ok {
			// no? let's copy it from the image config
			env = append(env, imageEnv)
		}
	}

	return env
}

func setPathFromImageConfig(env []string, imgEnv []string) []string {
	// check if user supplied PATH environment variable at creation time
	for _, v := range env {
		if strings.HasPrefix(v, "PATH=") {
			// a PATH is set, bail
			return env
		}
	}

	// check to see if the image this container is created from supplies a PATH
	for _, v := range imgEnv {
		if strings.HasPrefix(v, "PATH=") {
			// a PATH was found, add it to the config
			env = append(env, v)
			return env
		}
	}

	// no PATH set, use the default
	env = append(env, fmt.Sprintf("PATH=%s", defaultEnvPath))

	return env
}

// validateCreateConfig() checks the parameters for ContainerCreate().
// It may "fix up" the config param passed into ConntainerCreate() if needed.
func validateCreateConfig(config *types.ContainerCreateConfig) error {
	defer trace.End(trace.Begin("Container.validateCreateConfig"))

	if config.Config == nil {
		return engerr.BadRequestError("invalid config")
	}

	if config.HostConfig == nil {
		config.HostConfig = &container.HostConfig{}
	}

	// process cpucount here
	var cpuCount int64 = DefaultCPUs

	// support windows client
	if config.HostConfig.CPUCount > 0 {
		cpuCount = config.HostConfig.CPUCount
	} else {
		// we hijack --cpuset-cpus in the non-windows case
		if config.HostConfig.CpusetCpus != "" {
			cpus := strings.Split(config.HostConfig.CpusetCpus, ",")
			if c, err := strconv.Atoi(cpus[0]); err == nil {
				cpuCount = int64(c)
			} else {
				return fmt.Errorf("Error parsing CPU count: %s", err)
			}
		}
	}
	config.HostConfig.CPUCount = cpuCount

	// fix-up cpu/memory settings here
	if cpuCount < MinCPUs {
		config.HostConfig.CPUCount = MinCPUs
	}
	log.Infof("Container CPU count: %d", config.HostConfig.CPUCount)

	// convert from bytes to MiB for vsphere
	memoryMB := config.HostConfig.Memory / units.MiB
	if memoryMB == 0 {
		memoryMB = MemoryDefaultMB
	} else if memoryMB < MemoryMinMB {
		memoryMB = MemoryMinMB
	}

	// check that memory is aligned
	if remainder := memoryMB % MemoryAlignMB; remainder != 0 {
		log.Warnf("Default container VM memory must be %d aligned for hotadd, rounding up.", MemoryAlignMB)
		memoryMB += MemoryAlignMB - remainder
	}

	config.HostConfig.Memory = memoryMB
	log.Infof("Container memory: %d MB", config.HostConfig.Memory)

	////if config.NetworkingConfig == nil {
	////	config.NetworkingConfig = &dnetwork.NetworkingConfig{}
	////} else {
	////	if l := len(config.NetworkingConfig.EndpointsConfig); l > 1 {
	////		return fmt.Errorf("NetworkMode error: Container can be connected to one network endpoint only")
	////	}
	////	// If NetworkConfig exists, set NetworkMode to the default endpoint network, assuming only one endpoint network as the default network during container create
	////	for networkName := range config.NetworkingConfig.EndpointsConfig {
	////		config.HostConfig.NetworkMode = containertypes.NetworkMode(networkName)
	////	}
	////}
	//
	//// validate port bindings
	//var ips []string
	//if addrs, err := networking.PublicIPv4Addrs(); err != nil {
	//	log.Warnf("could not get address for public interface: %s", err)
	//} else {
	//	ips = make([]string, len(addrs))
	//	for i := range addrs {
	//		ips[i] = addrs[i]
	//	}
	//}
	//
	//for _, pbs := range config.HostConfig.PortBindings {
	//	for _, pb := range pbs {
	//		if pb.HostIP != "" && pb.HostIP != "0.0.0.0" {
	//			// check if specified host ip equals any of the addresses on the "client" interface
	//			found := false
	//			for _, i := range ips {
	//				if i == pb.HostIP {
	//					found = true
	//					break
	//				}
	//			}
	//			if !found {
	//				return engerr.InternalServerError("host IP for port bindings is only supported for 0.0.0.0 and the public interface IP address")
	//			}
	//		}
	//
	//		// #nosec: Errors unhandled.
	//		start, end, _ := nat.ParsePortRangeToInt(pb.HostPort)
	//		if start != end {
	//			return engerr.InternalServerError("host port ranges are not supported for port bindings")
	//		}
	//	}
	//}
	//
	//// https://github.com/vmware/vic/issues/1378
	//if len(config.Config.Entrypoint) == 0 && len(config.Config.Cmd) == 0 {
	//	return derr.NewRequestNotFoundError(fmt.Errorf("No command specified"))
	//}

	return nil
}