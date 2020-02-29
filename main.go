// This Source Code Form is subject to the terms of the Mozilla Public
// License, version 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	githubToken       string
	githubUser        string
	githubRepo        string
	githubAPIEndpoint string
	// Version gets initialized in compilation time.
	Version string
	debug   bool
)

type Asset struct {
	Id    int    `json:"id"`
	Name  string `json:"name"`
	Label string `json:"label"`
	State string `json:"state"`
	Size  int64  `json:"size"`
}

// Release represents a Github Release.
type Release struct {
	UploadURL  string  `json:"upload_url,omitempty"`
	TagName    string  `json:"tag_name"`
	Branch     string  `json:"target_commitish"`
	Name       string  `json:"name"`
	Body       string  `json:"body"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Id         int     `json:"id"`
	Assets     []Asset `json:"assets,omitempty"`
}

var verFlag bool
var prereleaseFlag bool
var draftFlag bool

func init() {
	log.SetFlags(0)

	debug, _ = strconv.ParseBool(os.Getenv("DEBUG"))

	githubToken = os.Getenv("GITHUB_TOKEN")
	githubUser = os.Getenv("GITHUB_USER")
	githubRepo = os.Getenv("GITHUB_REPO")
	githubAPIEndpoint = os.Getenv("GITHUB_API")

	if githubAPIEndpoint == "" {
		githubAPIEndpoint = "https://api.github.com"
	}

	flag.BoolVar(&verFlag, "version", false, "-version")
	flag.BoolVar(&prereleaseFlag, "prerelease", false, "-prerelease")
	flag.BoolVar(&draftFlag, "draft", false, "-draft")
	flag.Parse()
}

var usage = `Github command line release tool.

Usage:
	github-release <user/repo> <tag> <branch> <description> "<files>"

Parameters:
	<user/repo>: Github user and repository
	<tag>: Used to created the release. It is also used as the release's name
	<branch>: Reference from where to create the provided <tag>, if it does not exist
	<description>: The release description
	<files>: Glob pattern describing the list of files to include in the release.
	Make sure you enclose it in quotes to avoid the shell expanding the glob pattern.

Options:
	-version: Displays version
	-prerelease: Identify the release as a prerelease
	-draft: Save as draft, don't publish

Environment variables:
  DEBUG: Allows you to run github-release in debugging mode. DO NOT do this if you are attempting to upload big files.
  GITHUB_TOKEN: Must be set in order to interact with Github's API
  GITHUB_USER: Just in case you want an alternative way of providing your github user
  GITHUB_REPO: Just in case you want an alternative way of providing your github repo
  GITHUB_API: Github API endpoint. Set to https://api.github.com/repos/:github-user/:github-repo by default

Before using this tool make sure you set the environment variable GITHUB_TOKEN
with a valid Github token and correct authorization scopes to allow you to create releases
in your project. For more information about creating Github tokens please read the
official Github documentation at https://help.github.com/articles/creating-an-access-token-for-command-line-use/

Author: https://github.com/c4milo
License: http://mozilla.org/MPL/2.0/
`

func main() {
	if verFlag {
		log.Println(Version)
		return
	}

	if flag.NArg() != 5 {
		log.Printf("Error: Invalid number of arguments (got %d, expected 5)\n\n", flag.NArg())
		log.Fatal(usage)
	}

	userRepo := strings.Split(flag.Arg(0), "/")
	if len(userRepo) != 2 {
		log.Printf("Error: Invalid format used for username and repository: %s\n\n", flag.Arg(0))
		log.Fatal(usage)
	}

	if githubToken == "" {
		log.Fatal(`Error: GITHUB_TOKEN environment variable is not set.
Please refer to https://help.github.com/articles/creating-an-access-token-for-command-line-use/ for more help`)
	}

	githubUser = userRepo[0]
	githubRepo = userRepo[1]
	githubAPIEndpoint = fmt.Sprintf("%s/repos/%s/%s", githubAPIEndpoint, githubUser, githubRepo)

	if debug {
		log.Println("Glob pattern received: ")
		log.Println(flag.Arg(4))
	}

	filepaths, err := filepath.Glob(flag.Arg(4))
	if err != nil {
		log.Fatalf("Error: Invalid glob pattern: %s\n", flag.Arg(4))
	}

	if debug {
		log.Println("Expanded glob pattern: ")
		log.Printf("%v\n", filepaths)
	}

	tag := flag.Arg(1)
	branch := flag.Arg(2)
	desc := flag.Arg(3)

	release := Release{
		TagName:    tag,
		Name:       tag,
		Prerelease: prereleaseFlag,
		Draft:      draftFlag,
		Branch:     branch,
		Body:       desc,
	}
	publishRelease(release, filepaths)
	log.Println("Done")
}

func uploadFile(uploadURL, path string) ([]byte, int, error) {
	file, err := os.Open(path)
	if err != nil {
		log.Printf("Error: %s\n", err.Error())
		return nil, 0, err
	}
	defer file.Close()

	size, err := fileSize(file)
	if err != nil {
		log.Printf("Error: %s\n", err.Error())
		return nil, 0, err
	}

	filename := filepath.Base(file.Name())
	log.Printf("Uploading %s...\n", filename)
	body, status, err := doRequest("POST", uploadURL+"?name="+filename, "application/octet-stream", file, size)

	if debug {
		log.Println("========= UPLOAD RESPONSE ===========")
		log.Println(string(body[:]))
	}

	return body, status, err
}

// CreateRelease creates a Github Release, attaching the given files as release assets
// If a release already exist, up in Github, this function will attempt to attach the given files to it.
func CreateRelease(tag, branch, desc string, filepaths []string) {
	release := Release{
		TagName:    tag,
		Name:       tag,
		Prerelease: false,
		Draft:      false,
		Branch:     branch,
		Body:       desc,
	}
	publishRelease(release, filepaths)
}

type AssetHasCorrectSizeError struct {
	Asset Asset
}

func (a AssetHasCorrectSizeError) Error() string {
	return fmt.Sprintf("asset %s has expected file size of %d", a.Asset.Name, a.Asset.Size)
}

func deleteAssetWithWrongFileSize(releaseTag string, fileName string, fileSize int64) error {
	asset, err := getAssetByFilename(releaseTag, fileName)
	if err != nil {
		return err
	}

	if asset.Size == fileSize {
		return AssetHasCorrectSizeError{Asset: asset}
	}

	log.Printf("asset %s has incorrect file size of %d; deleting it.\n", asset.Name, asset.Size)
	_, _, err = doRequest("DELETE", fmt.Sprintf("%s/releases/assets/%d", githubAPIEndpoint, asset.Id), "application/json", nil, int64(0))
	return err
}

func getAssetByFilename(releaseTag string, filename string) (Asset, error) {
	assetResult := Asset{}
	data, _, err := doRequest("GET", fmt.Sprintf("%s/releases/tags/%s", githubAPIEndpoint, releaseTag), "application/json", nil, int64(0))
	if err != nil {
		return assetResult, err
	}

	var release Release
	err = json.Unmarshal(data, &release)
	if err != nil {
		return assetResult, err
	}

	assetIndex := -1
	for i := range release.Assets {
		if release.Assets[i].Name == filename {
			assetIndex = i
			break
		}
	}

	if assetIndex == -1 {
		return assetResult, fmt.Errorf("could not find asset %s", filename)
	}

	assetResult = release.Assets[assetIndex]

	return assetResult, nil
}

func uploadFileWithRetry(uploadURL string, filePath string, releaseTag string, retryLimit int) error {

	fileStat, err := os.Stat(filePath)
	if err != nil {
		return err
	}

	fileSize := fileStat.Size()
	fileName := filepath.Base(filePath)
	waitTime := time.Second
	retryCount := 0

	// The loop only breaks if we see that the asset exists and has the correct size
	for {

		log.Printf("Deleting asset %s if it exists and has incorrect file size (expecting %d).\n", fileName, fileSize)
		err := deleteAssetWithWrongFileSize(releaseTag, fileName, fileSize)

		if _, ok := err.(AssetHasCorrectSizeError); ok {
			log.Printf("Asset %s exists and seems to have the correct file size of %d.\n", fileName, fileSize)
			break
		}

		if err != nil {
			log.Printf("Deleting asset failed, which is OK: %s\n", err.Error())
		}

		if retryCount >= retryLimit {
			return fmt.Errorf("Retry limit of %d reached.\n", retryLimit)
		}

		if retryCount > 0 {
			waitTime = waitTime * 2
			log.Printf("Retrying upload after waiting %d second(s).\n", waitTime/time.Second)
			time.Sleep(waitTime)
		}

		_, _, err = uploadFile(uploadURL, filePath)

		if err != nil {
			log.Printf("Failed to upload asset %s\n", filePath)
			log.Printf("Error: %s\n", err.Error())
		}

		// Regardless of whether the upload gave an error, we loop until we confirm that the asset exists and has
		// the correct size.
		retryCount++
	}

	return nil
}

func publishRelease(release Release, filepaths []string) {
	releaseData, err := json.Marshal(release)
	if err != nil {
		log.Fatalln(err)
	}

	releaseBuffer := bytes.NewBuffer(releaseData)

	data, _, err := doRequest("POST", fmt.Sprintf("%s/releases", githubAPIEndpoint), "application/json", releaseBuffer, int64(releaseBuffer.Len()))

	if err != nil && bytes.Contains(data, []byte("already_exists")) {
		log.Println(err)
		log.Println("Release already exists. Getting existing release info to attach assets.")
		data, _, err = doRequest("GET", fmt.Sprintf("%s/releases/tags/%s", githubAPIEndpoint, release.TagName), "application/json", nil, int64(0))
	}

	if err != nil {
		log.Fatalln(err)
	}

	// Gets the release Upload URL from the returned JSON data
	err = json.Unmarshal(data, &release)
	if err != nil {
		log.Fatalln(err)
	}

	// Upload URL comes like this https://uploads.github.com/repos/octocat/Hello-World/releases/1/assets{?name}
	// So we need to remove the {?name} part
	uploadURL := strings.Split(release.UploadURL, "{")[0]

	const retryLimit = 5
	for _, filePath := range filepaths {
		err := uploadFileWithRetry(uploadURL, filePath, release.TagName, retryLimit)
		if err != nil {
			log.Fatalln(err)
		}
	}
}

func fileSize(file *os.File) (int64, error) {
	stat, err := file.Stat()
	if err != nil {
		return 0, err
	}
	return stat.Size(), nil
}

// Sends HTTP request to Github API
func doRequest(method, url, contentType string, reqBody io.Reader, bodySize int64) ([]byte, int, error) {
	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Authorization", fmt.Sprintf("token %s", githubToken))
	req.Header.Set("Content-type", contentType)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.ContentLength = bodySize

	if debug {
		log.Println("================ REQUEST DUMP ==================")
		dump, err := httputil.DumpRequestOut(req, true)
		if err != nil {
			log.Println(err.Error())
		}
		log.Println(string(dump[:]))
	}

	resp, err := http.DefaultClient.Do(req)

	if debug {
		log.Println("================ RESPONSE DUMP ==================")
		dump, err := httputil.DumpResponse(resp, true)
		if err != nil {
			log.Println(err.Error())
		}
		log.Println(string(dump[:]))
	}

	if err != nil {
		return nil, 0, err
	}

	defer resp.Body.Close()

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return respBody, resp.StatusCode, fmt.Errorf("Github returned an error:\n Code: %s. \n Body: %s", resp.Status, respBody)
	}

	return respBody, resp.StatusCode, nil
}
