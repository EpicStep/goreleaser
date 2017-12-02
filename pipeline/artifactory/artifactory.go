// Package artifactory provides a Pipe that push to artifactory
package artifactory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/goreleaser/goreleaser/config"
	"github.com/goreleaser/goreleaser/context"
	"github.com/goreleaser/goreleaser/internal/buildtarget"
	"github.com/goreleaser/goreleaser/pipeline"
	"golang.org/x/sync/errgroup"

	"github.com/apex/log"
)

// artifactoryResponse reflects the response after an upload request
// to Artifactory.
type artifactoryResponse struct {
	Repo              string               `json:"repo,omitempty"`
	Path              string               `json:"path,omitempty"`
	Created           string               `json:"created,omitempty"`
	CreatedBy         string               `json:"createdBy,omitempty"`
	DownloadURI       string               `json:"downloadUri,omitempty"`
	MimeType          string               `json:"mimeType,omitempty"`
	Size              string               `json:"size,omitempty"`
	Checksums         artifactoryChecksums `json:"checksums,omitempty"`
	OriginalChecksums artifactoryChecksums `json:"originalChecksums,omitempty"`
	URI               string               `json:"uri,omitempty"`
}

// artifactoryChecksums reflects the checksums generated by
// Artifactory
type artifactoryChecksums struct {
	SHA1   string `json:"sha1,omitempty"`
	MD5    string `json:"md5,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// Pipe for Artifactory
type Pipe struct{}

// Description of the pipe
func (Pipe) Description() string {
	return "Releasing to Artifactory"
}

// Run the pipe
//
// Docs: https://www.jfrog.com/confluence/display/RTF/Artifactory+REST+API#ArtifactoryRESTAPI-Example-DeployinganArtifact
func (Pipe) Run(ctx *context.Context) error {
	instances := len(ctx.Config.Artifactories)
	if instances == 0 {
		return pipeline.Skip("artifactory section is not configured")
	}

	// Check if for every instance we have a the target,
	// the username and a secret (password or api key)
	// If not, we can skip this pipeline
	for i := 0; i < instances; i++ {
		if ctx.Config.Artifactories[i].Target == "" {
			return pipeline.Skip(fmt.Sprintf("artifactory section is not configured properly (missing target in artifactory %d)", i))
		}

		if ctx.Config.Artifactories[i].Username == "" {
			return pipeline.Skip(fmt.Sprintf("artifactory section is not configured properly (missing username in artifactory %d)", i))
		}

		envName := fmt.Sprintf("ARTIFACTORY_%d_SECRET", i)
		if os.Getenv(envName) == "" {
			return pipeline.Skip(fmt.Sprintf("missing secret for artifactory %d: %s", i, ctx.Config.Artifactories[i].Target))
		}
	}

	return doRun(ctx)
}

func doRun(ctx *context.Context) error {
	if !ctx.Publish {
		return pipeline.Skip("--skip-publish is set")
	}

	// Loop over all builds, because we want to publish
	// every build to Artifactory
	for _, build := range ctx.Config.Builds {
		if err := runPipeOnBuild(ctx, build); err != nil {
			return err
		}
	}

	return nil
}

// runPipeOnBuild runs the pipe for every configured build
func runPipeOnBuild(ctx *context.Context, build config.Build) error {
	sem := make(chan bool, ctx.Parallelism)
	var g errgroup.Group

	// Lets generate the build matrix, , because we want to publish
	// every target to Artifactory
	for _, target := range buildtarget.All(build) {
		sem <- true
		target := target
		build := build
		g.Go(func() error {
			defer func() {
				<-sem
			}()

			return doBuild(ctx, build, target)
		})
	}

	return g.Wait()
}

// doBuild runs the pipe action of the current build and the current target
// This is where the real action take place
func doBuild(ctx *context.Context, build config.Build, target buildtarget.Target) (err error) {
	binary, err := getBinaryForUploadPerBuild(ctx, target)
	if err != nil {
		return err
	}

	// Loop over all configured Artifactory instances

	instances := len(ctx.Config.Artifactories)
	for i := 0; i < instances; i++ {
		artifactory := ctx.Config.Artifactories[i]
		secret := os.Getenv(fmt.Sprintf("ARTIFACTORY_%d_SECRET", i))

		// Generate name of target
		uploadTarget, err := buildTargetName(ctx, artifactory, target)
		if err != nil {
			// We log the error, but continue the process
			// The next target name could be generated successfully
			log.WithError(err).Error("Artifactory: Error while building the target name")
			continue
		}

		// The upload url to Artifactory needs the binary name
		// Here we add the binary to the target url
		if !strings.HasPrefix(uploadTarget, "/") {
			uploadTarget += "/"
		}
		uploadTarget += binary.Name

		// Upload the binary to Artifactory
		file, err := os.Open(binary.Path)
		if err != nil {
			return err
		}
		defer func() { err = file.Close() }()

		artifact, resp, err := uploadBinaryToArtifactory(ctx, uploadTarget, artifactory.Username, secret, file)
		if err != nil {
			if resp != nil {
				log.WithError(err).Errorf("Artifactory: Upload to target %s failed (HTTP Status: %s)", uploadTarget, resp.Status)
			} else {
				log.WithError(err).Errorf("Artifactory: Upload to target %s failed", uploadTarget)
			}

			continue
		}

		log.WithField("uri", artifact.DownloadURI).WithField("target", target.PrettyString()).Info("uploaded successful")
	}

	return nil
}

// getBinaryForUploadPerBuild determines the correct binary
// for the upload
func getBinaryForUploadPerBuild(ctx *context.Context, target buildtarget.Target) (*context.Binary, error) {
	var group = ctx.Binaries[target.String()]
	if group == nil {
		return nil, fmt.Errorf("binary for build target %s not found", target.String())
	}

	var binary context.Binary
	for _, binaries := range group {
		for _, b := range binaries {
			binary = b
			break
		}
		break
	}

	return &binary, nil
}

// targetData is used as a template struct for
// Artifactory.Target
type targetData struct {
	Os          string
	Arch        string
	Arm         string
	Version     string
	Tag         string
	ProjectName string
}

// buildTargetName returns the name resolved target name with replaced variables
// Those variables can be replaced by the given context, goos, goarch, goarm and more
func buildTargetName(ctx *context.Context, artifactory config.Artifactory, target buildtarget.Target) (string, error) {
	data := targetData{
		Os:          replace(ctx.Config.Archive.Replacements, target.OS),
		Arch:        replace(ctx.Config.Archive.Replacements, target.Arch),
		Arm:         replace(ctx.Config.Archive.Replacements, target.Arm),
		Version:     ctx.Version,
		Tag:         ctx.Git.CurrentTag,
		ProjectName: ctx.Config.ProjectName,
	}
	var out bytes.Buffer
	t, err := template.New(ctx.Config.ProjectName).Parse(artifactory.Target)
	if err != nil {
		return "", err
	}
	err = t.Execute(&out, data)
	return out.String(), err
}

func replace(replacements map[string]string, original string) string {
	result := replacements[original]
	if result == "" {
		return original
	}
	return result
}

// uploadBinaryToArtifactory uploads the binary file to target
func uploadBinaryToArtifactory(ctx *context.Context, target, username, secret string, file *os.File) (*artifactoryResponse, *http.Response, error) {
	stat, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if stat.IsDir() {
		return nil, nil, errors.New("the asset to upload can't be a directory")
	}

	req, err := newUploadRequest(target, username, secret, file, stat.Size())
	if err != nil {
		return nil, nil, err
	}

	asset := new(artifactoryResponse)
	resp, err := executeHTTPRequest(ctx, req, asset)
	if err != nil {
		return nil, resp, err
	}
	return asset, resp, nil
}

// newUploadRequest creates a new http.Request for uploading
func newUploadRequest(target, username, secret string, reader io.Reader, size int64) (*http.Request, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("PUT", u.String(), reader)
	if err != nil {
		return nil, err
	}

	req.ContentLength = size
	req.SetBasicAuth(username, secret)

	return req, err
}

// executeHTTPRequest processes the http call with respect of context ctx
func executeHTTPRequest(ctx *context.Context, req *http.Request, v interface{}) (resp *http.Response, err error) {
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		// If we got an error, and the context has been canceled,
		// the context's error is probably more useful.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		return nil, err
	}

	defer func() {
		err = resp.Body.Close()
	}()

	err = checkResponse(resp)
	if err != nil {
		// even though there was an error, we still return the response
		// in case the caller wants to inspect it further
		return resp, err
	}

	err = json.NewDecoder(resp.Body).Decode(v)
	return resp, err
}

// An ErrorResponse reports one or more errors caused by an API request.
type errorResponse struct {
	Response *http.Response // HTTP response that caused this error
	Errors   []Error        `json:"errors"` // more detail on individual errors
}

func (r *errorResponse) Error() string {
	return fmt.Sprintf("%v %v: %d %+v",
		r.Response.Request.Method, r.Response.Request.URL,
		r.Response.StatusCode, r.Errors)
}

// An Error reports more details on an individual error in an ErrorResponse.
type Error struct {
	Status  int    `json:"status"`  // Error code
	Message string `json:"message"` // Message describing the error.
}

func (e *Error) Error() string {
	return fmt.Sprintf("%v (%v)", e.Message, e.Status)
}

// checkResponse checks the API response for errors, and returns them if
// present. A response is considered an error if it has a status code outside
// the 200 range.
// API error responses are expected to have either no response
// body, or a JSON response body that maps to ErrorResponse. Any other
// response body will be silently ignored.
func checkResponse(r *http.Response) error {
	if c := r.StatusCode; 200 <= c && c <= 299 {
		return nil
	}
	errorResponse := &errorResponse{Response: r}
	data, err := ioutil.ReadAll(r.Body)
	if err == nil && data != nil {
		err := json.Unmarshal(data, errorResponse)
		if err != nil {
			return err
		}
	}
	return errorResponse
}
