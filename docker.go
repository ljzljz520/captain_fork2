package captain // import "github.com/harbur/captain"

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/fsouza/go-dockerclient"
)

const defaultEndpoint = "unix:///var/run/docker.sock"

var client *docker.Client

func init() {
	var err error
	client, err = docker.NewClientFromEnv()
	if err != nil {
		panic(err)
	}
}

type BuildArgSet struct {
	slice []docker.BuildArg
}

func buildImage(app App, tag string, force bool) error {
	info("Building image %s:%s", app.Image, tag)

	// Nasty issue with CircleCI https://github.com/docker/docker/issues/4897
	if os.Getenv("CIRCLECI") == "true" {
		info("Running at %s environment...", "CIRCLECI")
		execute("docker", "build", "-t", app.Image+":"+tag, filepath.Dir(app.Build))
		return nil
	}

	// Create BuildArg set
	buildArgSet := BuildArgSet{(make([]docker.BuildArg, 0, 10))}
	if len(app.Build_arg) > 0 {
		for k, arg := range app.Build_arg {
			buildArgSet.slice = append(buildArgSet.slice, docker.BuildArg{Name: k, Value: arg})
		}
	}
	opts := docker.BuildImageOptions{
		Name:                app.Image + ":" + tag,
		Dockerfile:          filepath.Base(app.Build),
		NoCache:             force,
		SuppressOutput:      false,
		RmTmpContainer:      true,
		ForceRmTmpContainer: true,
		OutputStream:        os.Stdout,
		ContextDir:          filepath.Dir(app.Build),
		BuildArgs:           buildArgSet.slice,
	}

	// Use ~/.docker/ auth configuration if exists
	dockercfg, _ := docker.NewAuthConfigurationsFromDockerCfg()
	if dockercfg != nil {
		opts.AuthConfigs = *dockercfg
	}

	err := client.BuildImage(opts)
	if err != nil {
		fmt.Printf("%s", err)
	}
	return err
}

// inspectImageID resolves a name:tag reference to the immutable image ID
// ("sha256:...") held by the daemon.
func inspectImageID(repo string) (string, error) {
	image, err := client.InspectImage(repo)
	if err != nil {
		return "", err
	}
	return image.ID, nil
}

// buildImageUnique builds the app image under a per-build temporary tag
// instead of a shared tag such as "latest", then resolves the immutable
// image ID. Concurrent builds on the same daemon therefore get independent
// artifacts that cannot overwrite each other; the returned buildRecord is
// the only identity handed to later test/push steps.
func buildImageUnique(app App, force bool) (buildRecord, error) {
	buildTag := newBuildTag()
	if err := buildImage(app, buildTag, force); err != nil {
		return buildRecord{}, err
	}
	id, err := inspectImageID(app.Image + ":" + buildTag)
	if err != nil {
		return buildRecord{}, fmt.Errorf("built %s:%s but failed to resolve its immutable image ID: %v",
			app.Image, buildTag, err)
	}
	return buildRecord{Image: app.Image, ID: id, BuildTag: buildTag}, nil
}

// verifyTagPointsTo re-inspects a shared tag and confirms it still resolves
// to the immutable image ID recorded for this command. Returns
// *ExternalRetagError on drift, so callers can distinguish external retag
// from ordinary daemon failures.
func verifyTagPointsTo(app App, tag, expectedID string) error {
	actual, err := inspectImageID(app.Image + ":" + tag)
	if err != nil {
		return fmt.Errorf("cannot verify %s:%s before using it: %v", app.Image, tag, err)
	}
	if actual != expectedID {
		return &ExternalRetagError{
			Image:      app.Image,
			Tag:        tag,
			ExpectedID: expectedID,
			ActualID:   actual,
		}
	}
	return nil
}

func pushImage(image string, version string) error {
	return execute("docker", "push", image+":"+version)
}

func pullImage(image string, version string) error {
	return execute("docker", "pull", image+":"+version)
}

func tagImage(app App, origin string, tag string) error {
	if tag != "" {
		info("Tagging image %s:%s as %s:%s", app.Image, origin, app.Image, tag)
		opts := docker.TagImageOptions{Repo: app.Image, Tag: tag, Force: true}
		err := client.TagImage(app.Image+":"+origin, opts)
		if err != nil {
			fmt.Printf("%s", err)
		}
		return err
	}

	debug("Skipping tag of %s - no git repository", app.Image)

	return nil
}

func removeImage(name string) error {
	return client.RemoveImage(name)
}

/**
 * Retrieves a list of existing Images for the specific App.
 */
func getImages(app App) []docker.APIImages {
	debug("Getting images %s", app.Image)
	imgs, _ := client.ListImages(docker.ListImagesOptions{All: false, Filter: app.Image})
	return imgs
}

func imageExist(app App, tag string) bool {
	repo := app.Image + ":" + tag
	image, _ := client.InspectImage(repo)
	if image != nil {
		return true
	}
	return false
}
