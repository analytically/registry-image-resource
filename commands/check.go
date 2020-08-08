package commands

import (
	"encoding/json"
	"fmt"
	resource "github.com/concourse/registry-image-resource"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/sirupsen/logrus"
	"io"
)

type CheckRequest struct {
	Source  resource.Source   `json:"source"`
	Version *resource.Version `json:"version"`
}

type CheckResponse []resource.Version

type check struct {
	stdin  io.Reader
	stderr io.Writer
	stdout io.Writer
	args   []string
}

func NewCheck(
	stdin io.Reader,
	stderr io.Writer,
	stdout io.Writer,
	args []string,
) *check {
	return &check{
		stdin:  stdin,
		stderr: stderr,
		stdout: stdout,
		args:   args,
	}
}

func (c *check) Execute() error {
	setupLogging(c.stderr)

	var req CheckRequest
	decoder := json.NewDecoder(c.stdin)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&req)
	if err != nil {
		return fmt.Errorf("invalid payload: %s", err)
	}

	if req.Source.AwsAccessKeyId != "" && req.Source.AwsSecretAccessKey != "" && req.Source.AwsRegion != "" {
		if !req.Source.AuthenticateToECR() {
			return fmt.Errorf("cannot authenticate with ECR")
		}
	}

	repo, err := name.NewRepository(req.Source.Repository, name.WeakValidation)
	if err != nil {
		return fmt.Errorf("failed to resolve repository: %s", err)
	}

	var response CheckResponse
	tag := new(name.Tag)

	if req.Source.RegistryMirror != nil {
		mirror, err := name.NewRepository(repo.String())
		if err != nil {
			return fmt.Errorf("could not resolve mirror repository: %s", err)
		}

		mirror.Registry, err = name.NewRegistry(req.Source.RegistryMirror.Host, name.WeakValidation)
		if err != nil {
			return fmt.Errorf("could not resolve registry: %s", err)
		}

		*tag = mirror.Tag(req.Source.Tag())

		response, err = checkWithRetry(req.Source.RegistryMirror.BasicCredentials, req.Version, *tag)
		if err != nil {
			logrus.Warnf("checking mirror %s failed: %s", mirror.RegistryStr(), err)
		} else if len(response) == 0 {
			logrus.Warnf("checking mirror %s failed: tag not found", mirror.RegistryStr())
		}
	}

	if len(response) == 0 {
		*tag = repo.Tag(req.Source.Tag())
		response, err = checkWithRetry(req.Source.BasicCredentials, req.Version, *tag)
	}
	if err != nil {
		return fmt.Errorf("checking origin %s failed: %s", tag.RegistryStr(), err)
	}

	err =  json.NewEncoder(c.stdout).Encode(response)
	if err != nil {
		return fmt.Errorf("could not marshal JSON: %s", err)
	}

	return nil
}

func checkWithRetry(principal resource.BasicCredentials, version *resource.Version, ref name.Tag) (CheckResponse, error) {
	var response CheckResponse
	err := resource.RetryOnRateLimit(func() error {
		var err error
		response, err = performCheck(principal, version, ref)
		return err
	})
	return response, err
}

func performCheck(principal resource.BasicCredentials, version *resource.Version, ref name.Tag) (CheckResponse, error) {
	auth := &authn.Basic{
		Username: principal.Username,
		Password: principal.Password,
	}

	imageOpts := []remote.Option{}

	if auth.Username != "" && auth.Password != "" {
		imageOpts = append(imageOpts, remote.WithAuth(auth))
	}

	var missingTag bool
	image, err := remote.Image(ref, imageOpts...)
	if err != nil {
		missingTag = checkMissingManifest(err)
		if !missingTag {
			return CheckResponse{}, fmt.Errorf("get remote image: %w", err)
		}
	}

	var digest v1.Hash
	if !missingTag {
		digest, err = image.Digest()
		if err != nil {
			return CheckResponse{}, fmt.Errorf("get cursor image digest: %w", err)
		}
	}

	response := CheckResponse{}
	if version != nil && !missingTag && version.Digest != digest.String() {
		digestRef := ref.Repository.Digest(version.Digest)

		digestImage, err := remote.Image(digestRef, imageOpts...)
		var missingDigest bool
		if err != nil {
			missingDigest = checkMissingManifest(err)
			if !missingDigest {
				return CheckResponse{}, fmt.Errorf("get remote image: %w", err)
			}
		}

		if !missingDigest {
			_, err = digestImage.Digest()
			if err != nil {
				return CheckResponse{}, fmt.Errorf("get cursor image digest: %w", err)
			}

			response = append(response, *version)
		}
	}

	if !missingTag {
		response = append(response, resource.Version{
			Digest: digest.String(),
		})
	}

	return response, nil
}

func checkMissingManifest(err error) bool {
	var missing bool
	if rErr, ok := err.(*transport.Error); ok {
		for _, e := range rErr.Errors {
			if e.Code == transport.ManifestUnknownErrorCode {
				missing = true
				break
			}
		}
	}
	return missing
}
