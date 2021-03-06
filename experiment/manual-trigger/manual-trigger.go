/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// manual-trigger triggers jenkins jobs based a specified github pull request
package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/url"
	"strings"

	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/jenkins"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/pjutil"
)

type options struct {
	githubEndpoint         string
	githubTokenFile        string
	jenkinsBearerTokenFile string
	jenkinsURL             string
	jenkinsTokenFile       string
	jenkinsUserName        string
	jobName                string
	num                    int
	org                    string
	repo                   string
}

func flagOptions() options {
	o := options{}

	flag.StringVar(&o.jenkinsBearerTokenFile, "jenkins-bearer-token-file", "", "Path to the file containing the Jenkins API bearer token.")
	flag.StringVar(&o.jenkinsURL, "jenkins-url", "", "Jenkins URL.")
	flag.StringVar(&o.jenkinsTokenFile, "jenkins-token-file", "", "Path to the file containing the Jenkins API token.")
	flag.StringVar(&o.jenkinsUserName, "jenkins-user-name", "", "Jenkins username.")

	flag.StringVar(&o.githubEndpoint, "github-endpoint", "https://api.github.com", "GitHub's API endpoint.")
	flag.StringVar(&o.githubTokenFile, "github-token-file", "", "Path to file containing GitHub OAuth token.")

	flag.StringVar(&o.jobName, "job-name", "", "Name of Jenkins job")

	flag.IntVar(&o.num, "num", 0, "GitHub issue number")
	flag.StringVar(&o.org, "org", "", "GitHub organization")
	flag.StringVar(&o.repo, "repo", "", "GitHub repository")
	flag.Parse()
	return o
}

func sanityCheckFlags(o options) error {
	if o.num <= 0 {
		return fmt.Errorf("empty or invalid --num")
	}
	if o.org == "" {
		return fmt.Errorf("empty --org")
	}
	if o.repo == "" {
		return fmt.Errorf("empty --repo")
	}
	if o.githubTokenFile == "" {
		return fmt.Errorf("empty --github-token-file")
	}
	if o.jobName == "" {
		return fmt.Errorf("empty --job-name")
	}

	if o.jenkinsBearerTokenFile == "" && (o.jenkinsUserName == "" || o.jenkinsTokenFile == "") {
		return fmt.Errorf("neither --jenkins-bearer-token-file nor the combination of --jenkins-user-name and --jenkins-token-file were provided")
	}

	if o.githubEndpoint == "" {
		return fmt.Errorf("empty --github-endpoint")
	} else if _, err := url.Parse(o.githubEndpoint); err != nil {
		return fmt.Errorf("bad --github-endpoint provided: %v", err)
	}

	if o.jenkinsURL == "" {
		return fmt.Errorf("empty --jenkins-url")
	} else if _, err := url.Parse(o.jenkinsURL); err != nil {
		return fmt.Errorf("bad --jenkins-url provided: %v", err)
	}

	return nil
}

func loadToken(path string) (string, error) {
	raw, err := ioutil.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func main() {
	o := flagOptions()
	err := sanityCheckFlags(o)
	if err != nil {
		log.Fatal(err)
	}

	// TODO(kargakis): dry this out
	ac := jenkins.AuthConfig{}
	if o.jenkinsTokenFile != "" {
		token, err := loadToken(o.jenkinsTokenFile)
		if err != nil {
			log.Fatalf("cannot read file specified by --jenkins-token-file: %v", err)
		}
		ac.Basic = &jenkins.BasicAuthConfig{
			User:  o.jenkinsUserName,
			Token: token,
		}
	} else if o.jenkinsBearerTokenFile != "" {
		token, err := loadToken(o.jenkinsBearerTokenFile)
		if err != nil {
			log.Fatalf("cannot read file specified by --jenkins-bearer-token-file: %v", err)
		}
		ac.BearerToken = &jenkins.BearerTokenAuthConfig{
			Token: token,
		}
	} else {
		log.Fatalf("no jenkins auth token provided")
	}

	jc := jenkins.NewClient(o.jenkinsURL, &ac, nil)

	token, err := loadToken(o.githubTokenFile)
	if err != nil {
		log.Fatalf("cannot read file specified by --ghtoken: %v", err)
	}

	gc := github.NewClient(token, o.githubEndpoint)

	pr, err := gc.GetPullRequest(o.org, o.repo, o.num)
	if err != nil {
		log.Fatalf("Unable to get information on pull request %s/%s#%d: %v", o.org, o.repo, o.num, err)
	}

	spec := kube.ProwJobSpec{
		Type: kube.PresubmitJob,
		Job:  o.jobName,
		Refs: kube.Refs{
			Org:     o.org,
			Repo:    o.repo,
			BaseRef: pr.Base.Ref,
			BaseSHA: pr.Base.SHA,
			Pulls: []kube.Pull{
				{
					Number: pr.Number,
					Author: pr.User.Login,
					SHA:    pr.Head.SHA,
				},
			},
		},

		Report:         false,
		Context:        "",
		RerunCommand:   "",
		MaxConcurrency: 1,
	}

	if err = jc.BuildFromSpec(&spec, o.jobName); err != nil {
		log.Println("Submitting the following to Jenkins:")
		env, _ := pjutil.EnvForSpec(pjutil.NewJobSpec(spec, "0"))
		for k, v := range env {
			log.Printf("  %s=%s\n", k, v)
		}
		log.Fatalf("for %s/%s#%d resulted in an error: %v", o.org, o.repo, o.num, err)
	} else {
		slash := "/"
		if strings.HasSuffix(o.jenkinsURL, "/") {
			slash = ""
		}
		log.Printf("Successfully submitted job to %s%sjob/%s", o.jenkinsURL, slash, o.jobName)
	}
}
