package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/yourbase/narwhal"
	"github.com/yourbase/yb"
	"github.com/yourbase/yb/internal/biome"
	"github.com/yourbase/yb/internal/build"
	"github.com/yourbase/yb/internal/ybdata"
	"zombiezen.com/go/log"
)

func connectDockerClient(useDocker bool) (*docker.Client, error) {
	if !useDocker {
		return nil, nil
	}
	dockerClient, err := docker.NewVersionedClient("unix:///var/run/docker.sock", "1.39")
	if err != nil {
		return nil, err
	}
	return dockerClient, nil
}

const netrcFilename = ".netrc"

type newBiomeOptions struct {
	packageDir string
	target     string
	dataDirs   *ybdata.Dirs
	baseEnv    biome.Environment

	dockerClient    *docker.Client
	targetContainer *narwhal.ContainerDefinition
	dockerNetworkID string
}

func (opts newBiomeOptions) disableDocker() newBiomeOptions {
	// Operating on copy, so free to modify fields.
	opts.dockerClient = nil
	opts.targetContainer = nil
	opts.dockerNetworkID = ""
	return opts
}

func newBiome(ctx context.Context, opts newBiomeOptions) (biome.BiomeCloser, error) {
	if opts.dockerClient == nil {
		// l := biome.Local{
		// 	PackageDir: opts.packageDir,
		// }
		// var err error
		// l.HomeDir, err = opts.dataDirs.BuildHome(opts.packageDir, opts.target, l.Describe())
		// if err != nil {
		// 	return nil, fmt.Errorf("set up environment for target %s: %w", opts.target, err)
		// }
		homeDir, err := opts.dataDirs.BuildHome(opts.packageDir, opts.target, biome.Local{}.Describe())
		if err != nil {
			return nil, fmt.Errorf("set up environment for target %s: %w", opts.target, err)
		}
		log.Debugf(ctx, "Home located at %s", homeDir)

		cb, err := newContainerBiome(ctx, opts.dataDirs, opts.packageDir, homeDir, opts.targetContainer.Image)
		if err != nil {
			return nil, fmt.Errorf("set up environment for target %s: %w", opts.target, err)
		}
		bio, err := injectNetrc(ctx, cb)
		if err != nil {
			return nil, fmt.Errorf("set up environment for target %s: %w", opts.target, err)
		}
		return biome.EnvBiome{
			Biome: bio,
			Env:   opts.baseEnv,
		}, nil
	}

	home, err := opts.dataDirs.BuildHome(opts.packageDir, opts.target, biome.DockerDescriptor())
	if err != nil {
		return nil, fmt.Errorf("set up environment for target %s: %w", opts.target, err)
	}
	log.Debugf(ctx, "Home located at %s", home)
	tiniFile, err := ybdata.Download(ctx, http.DefaultClient, opts.dataDirs, biome.TiniURL)
	if err != nil {
		return nil, fmt.Errorf("set up environment for target %s: %w", opts.target, err)
	}
	defer tiniFile.Close()
	c, err := biome.CreateContainer(ctx, opts.dockerClient, &biome.ContainerOptions{
		PackageDir: opts.packageDir,
		HomeDir:    home,
		TiniExe:    tiniFile,
		Definition: opts.targetContainer,
		NetworkID:  opts.dockerNetworkID,
		PullOutput: os.Stderr,
	})
	if err != nil {
		return nil, fmt.Errorf("set up environment for target %s: %w", opts.target, err)
	}
	bio, err := injectNetrc(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("set up environment for target %s: %w", opts.target, err)
	}
	return biome.EnvBiome{
		Biome: bio,
		Env:   opts.baseEnv,
	}, nil
}

func injectNetrc(ctx context.Context, bio biome.BiomeCloser) (biome.BiomeCloser, error) {
	const gitHubTokenVar = "YB_GH_TOKEN"
	token := os.Getenv(gitHubTokenVar)
	if token == "" {
		return bio, nil
	}
	log.Infof(ctx, "Writing .netrc")
	netrcPath := bio.JoinPath(bio.Dirs().Home, netrcFilename)
	err := biome.WriteFile(ctx, bio, netrcPath, bytes.NewReader(generateNetrc(token)))
	if err != nil {
		return nil, fmt.Errorf("write netrc: %w", err)
	}
	err = runCommand(ctx, bio, "chmod", "600", netrcPath)
	if err != nil {
		// Not fatal. File will be removed later.
		log.Warnf(ctx, "Making temporary .netrc private: %v", err)
	}
	bio = biome.WithClose(bio, func() error {
		ctx := context.Background()
		err := runCommand(ctx, bio,
			"rm", bio.JoinPath(bio.Dirs().Home, netrcFilename))
		if err != nil {
			log.Warnf(ctx, "Could not clean up .netrc: %v", err)
		}
		return nil
	})
	return biome.EnvBiome{
		Biome: bio,
		Env: biome.Environment{
			Vars: map[string]string{
				gitHubTokenVar: token,
			},
		},
	}, nil
}

func generateNetrc(gitHubToken string) []byte {
	if gitHubToken == "" {
		return nil
	}
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "machine github.com login x-access-token password %s\n", gitHubToken)
	return buf.Bytes()
}

func runCommand(ctx context.Context, bio biome.Biome, argv ...string) error {
	output := new(strings.Builder)
	err := bio.Run(ctx, &biome.Invocation{
		Argv:   argv,
		Stdout: output,
		Stderr: output,
	})
	if err != nil {
		if output.Len() > 0 {
			return fmt.Errorf("%w\n%s", err, output)
		}
		return err
	}
	return nil
}

func targetToPhaseDeps(target *yb.BuildTarget) (*build.PhaseDeps, error) {
	phaseDeps := &build.PhaseDeps{
		TargetName: target.Name,
		Resources:  narwhalContainerMap(target.Dependencies.Containers),
	}
	for _, dep := range target.Dependencies.Build {
		spec, err := yb.ParseBuildpackSpec(dep)
		if err != nil {
			return nil, fmt.Errorf("target %s: %w", target.Name, err)
		}
		phaseDeps.Buildpacks = append(phaseDeps.Buildpacks, spec)
	}
	var err error
	phaseDeps.EnvironmentTemplate, err = biome.MapVars(target.Environment)
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", target.Name, err)
	}
	return phaseDeps, nil
}

func narwhalContainerMap(defs map[string]*yb.ContainerDefinition) map[string]*narwhal.ContainerDefinition {
	if len(defs) == 0 {
		return nil
	}
	nmap := make(map[string]*narwhal.ContainerDefinition, len(defs))
	for k, cd := range defs {
		nmap[k] = cd.ToNarwhal()
	}
	return nmap
}

func targetToPhase(target *yb.BuildTarget) *build.Phase {
	return &build.Phase{
		TargetName: target.Name,
		Commands:   target.Commands,
		Root:       target.Root,
	}
}

func newDockerNetwork(ctx context.Context, client *docker.Client) (string, func(), error) {
	if client == nil {
		return "", func() {}, nil
	}
	var bits [8]byte
	if _, err := rand.Read(bits[:]); err != nil {
		return "", nil, fmt.Errorf("create docker network: generate name: %w", err)
	}
	name := hex.EncodeToString(bits[:])
	log.Infof(ctx, "Creating Docker network %s...", name)
	network, err := client.CreateNetwork(docker.CreateNetworkOptions{
		Context: ctx,
		Name:    name,
		Driver:  "bridge",
	})
	if err != nil {
		return "", nil, fmt.Errorf("create docker network: %w", err)
	}
	id := network.ID
	return id, func() {
		log.Infof(ctx, "Removing Docker network %s...", name)
		if err := client.RemoveNetwork(id); err != nil {
			log.Warnf(ctx, "Unable to remove Docker network %s (%s): %v", name, id, err)
		}
	}, nil
}

const packageConfigFileName = ".yourbase.yml"

func GetTargetPackage() (*yb.Package, error) {
	return yb.LoadPackage(packageConfigFileName)
}
