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

package plank

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"time"

	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/jenkins"
	"k8s.io/test-infra/prow/kube"
)

const (
	guberBasePR   = "https://k8s-gubernator.appspot.com/build/kubernetes-jenkins/pr-logs/pull"
	guberBasePush = "https://k8s-gubernator.appspot.com/build/kubernetes-jenkins/logs"
	testInfra     = "https://github.com/kubernetes/test-infra/issues"
)

type kubeClient interface {
	CreateProwJob(kube.ProwJob) (kube.ProwJob, error)
	ListProwJobs(map[string]string) ([]kube.ProwJob, error)
	ReplaceProwJob(string, kube.ProwJob) (kube.ProwJob, error)

	CreatePod(kube.Pod) (kube.Pod, error)
	ListPods(map[string]string) ([]kube.Pod, error)
	DeletePod(string) error
}

type jenkinsClient interface {
	Build(jenkins.BuildRequest) (*jenkins.Build, error)
	Enqueued(string) (bool, error)
	Status(job, id string) (*jenkins.Status, error)
}

type githubClient interface {
	BotName() string
	CreateStatus(org, repo, ref string, s github.Status) error
	ListIssueComments(org, repo string, number int) ([]github.IssueComment, error)
	CreateComment(org, repo string, number int, comment string) error
	DeleteComment(org, repo string, ID int) error
	EditComment(org, repo string, ID int, comment string) error
}

type Controller struct {
	kc     kubeClient
	pkc    kubeClient
	jc     jenkinsClient
	ghc    githubClient
	totURL string

	reports []kube.ProwJob
}

func NewController(kc, pkc *kube.Client, jc *jenkins.Client, ghc *github.Client, totURL string) *Controller {
	return &Controller{
		kc:     kc,
		pkc:    pkc,
		jc:     jc,
		ghc:    ghc,
		totURL: totURL,
	}
}

func (c *Controller) Sync() error {
	c.reports = []kube.ProwJob{}
	pjs, err := c.kc.ListProwJobs(nil)
	if err != nil {
		return fmt.Errorf("error listing prow jobs: %v", err)
	}
	pods, err := c.pkc.ListPods(nil)
	if err != nil {
		return fmt.Errorf("error listing pods: %v", err)
	}
	pm := map[string]kube.Pod{}
	for _, pod := range pods {
		pm[pod.Metadata.Name] = pod
	}
	var syncErrs []error
	if err := c.terminateDupes(pjs); err != nil {
		syncErrs = append(syncErrs, err)
	}
	for _, pj := range pjs {
		if pj.Spec.Agent == kube.KubernetesAgent {
			if err := c.syncKubernetesJob(pj, pm); err != nil {
				syncErrs = append(syncErrs, err)
			}
		} else if pj.Spec.Agent == kube.JenkinsAgent {
			if err := c.syncJenkinsJob(pj); err != nil {
				syncErrs = append(syncErrs, err)
			}
		} else {
			syncErrs = append(syncErrs, fmt.Errorf("job %s has unsupported agent %s", pj.Metadata.Name, pj.Spec.Agent))
		}
	}
	var reportErrs []error
	for _, pj := range c.reports {
		if err := c.report(pj); err != nil {
			reportErrs = append(reportErrs, err)
		}
	}
	if len(syncErrs) == 0 && len(reportErrs) == 0 {
		return nil
	}
	return fmt.Errorf("errors syncing: %v, errors reporting: %v", syncErrs, reportErrs)
}

// terminateDupes aborts presubmits that have a newer version.
func (c *Controller) terminateDupes(pjs []kube.ProwJob) error {
	// "job org/repo#number" -> newest job
	dupes := make(map[string]kube.ProwJob)
	for _, pj := range pjs {
		if pj.Complete() || pj.Spec.Type != kube.PresubmitJob {
			continue
		}
		n := fmt.Sprintf("%s %s/%s#%d", pj.Spec.Job, pj.Spec.Refs.Org, pj.Spec.Refs.Repo, pj.Spec.Refs.Pulls[0].Number)
		prev, ok := dupes[n]
		if !ok {
			dupes[n] = pj
			continue
		}
		toCancel := pj
		if prev.Status.StartTime.Before(pj.Status.StartTime) {
			toCancel = prev
			dupes[n] = pj
		}
		toCancel.Status.CompletionTime = time.Now()
		toCancel.Status.State = kube.AbortedState
		if _, err := c.kc.ReplaceProwJob(toCancel.Metadata.Name, toCancel); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) syncJenkinsJob(pj kube.ProwJob) error {
	var jerr error
	if pj.Complete() {
		return nil
	} else if pj.Status.State == kube.TriggeredState {
		// Start the Jenkins job.
		pj.Status.State = kube.PendingState
		br := jenkins.BuildRequest{
			JobName: pj.Spec.Job,
			Refs:    pj.Spec.Refs.String(),
			BaseRef: pj.Spec.Refs.BaseRef,
			BaseSHA: pj.Spec.Refs.BaseSHA,
		}
		if len(pj.Spec.Refs.Pulls) == 1 {
			br.Number = pj.Spec.Refs.Pulls[0].Number
			br.PullSHA = pj.Spec.Refs.Pulls[0].SHA
		}
		if build, err := c.jc.Build(br); err != nil {
			jerr = fmt.Errorf("error starting Jenkins job: %v", err)
			pj.Status.CompletionTime = time.Now()
			pj.Status.State = kube.ErrorState
			pj.Status.URL = testInfra
			pj.Status.Description = "Error starting Jenkins job."
		} else {
			pj.Status.JenkinsQueueURL = build.QueueURL.String()
			pj.Status.JenkinsBuildID = build.ID
			pj.Status.JenkinsEnqueued = true
			pj.Status.Description = "Jenkins job triggered."
		}
		c.reports = append(c.reports, pj)
	} else if pj.Status.JenkinsEnqueued {
		if eq, err := c.jc.Enqueued(pj.Status.JenkinsQueueURL); err != nil {
			jerr = fmt.Errorf("error checking queue status: %v", err)
			pj.Status.JenkinsEnqueued = false
			pj.Status.CompletionTime = time.Now()
			pj.Status.State = kube.ErrorState
			pj.Status.URL = testInfra
			pj.Status.Description = "Error checking queue status."
			c.reports = append(c.reports, pj)
		} else if eq {
			// Still in queue.
			return nil
		} else {
			pj.Status.JenkinsEnqueued = false
		}
	} else if status, err := c.jc.Status(pj.Spec.Job, pj.Status.JenkinsBuildID); err != nil {
		jerr = fmt.Errorf("error checking build status: %v", err)
		pj.Status.CompletionTime = time.Now()
		pj.Status.State = kube.ErrorState
		pj.Status.URL = testInfra
		pj.Status.Description = "Error checking job status."
		c.reports = append(c.reports, pj)
	} else {
		if url := guberURL(pj, strconv.Itoa(status.Number)); pj.Status.URL != url {
			pj.Status.URL = url
			pj.Status.PodName = fmt.Sprintf("%s-%d", pj.Spec.Job, status.Number)
		} else if status.Building {
			// Build still going.
			return nil
		}
		if !status.Building && status.Success {
			pj.Status.CompletionTime = time.Now()
			pj.Status.State = kube.SuccessState
			pj.Status.Description = "Jenkins job succeeded."
			for _, nj := range pj.Spec.RunAfterSuccess {
				if _, err := c.kc.CreateProwJob(NewProwJob(nj)); err != nil {
					return fmt.Errorf("error starting next prowjob: %v", err)
				}
			}
		} else if !status.Building {
			pj.Status.CompletionTime = time.Now()
			pj.Status.State = kube.FailureState
			pj.Status.Description = "Jenkins job failed."
		}
		c.reports = append(c.reports, pj)
	}
	_, rerr := c.kc.ReplaceProwJob(pj.Metadata.Name, pj)
	if rerr != nil || jerr != nil {
		return fmt.Errorf("jenkins error: %v, error replacing prow job: %v", jerr, rerr)
	}
	return nil
}

func (c *Controller) syncKubernetesJob(pj kube.ProwJob, pm map[string]kube.Pod) error {
	if pj.Complete() {
		if pj.Status.PodName == "" {
			// Completed ProwJob, already cleaned up the pod. Nothing to do.
			return nil
		} else if _, ok := pm[pj.Status.PodName]; ok {
			// Delete the old pod.
			if err := c.pkc.DeletePod(pj.Status.PodName); err != nil {
				return fmt.Errorf("error deleting pod %s: %v", pj.Status.PodName, err)
			}
		}
		pj.Status.PodName = ""
	} else if pj.Status.PodName == "" {
		// We haven't started the pod yet. Do so.
		pj.Status.State = kube.PendingState
		if id, pn, err := c.startPod(pj); err == nil {
			pj.Status.PodName = pn
			pj.Status.URL = guberURL(pj, id)
		} else {
			return fmt.Errorf("error starting pod: %v", err)
		}
		pj.Status.Description = "Job triggered."
		c.reports = append(c.reports, pj)
	} else if pod, ok := pm[pj.Status.PodName]; !ok {
		// Pod is missing. This shouldn't happen normally, but if someone goes
		// in and manually deletes the pod then we'll hit it. Start a new pod.
		pj.Status.PodName = ""
		pj.Status.State = kube.PendingState
	} else if pod.Status.Phase == kube.PodUnknown {
		// Pod is in Unknown state. This can happen if there is a problem with
		// the node. Delete the old pod, we'll start a new one next loop.
		if err := c.pkc.DeletePod(pj.Status.PodName); err != nil {
			return fmt.Errorf("error deleting pod %s: %v", pj.Status.PodName, err)
		}
		pj.Status.PodName = ""
		pj.Status.State = kube.PendingState
	} else if pod.Status.Phase == kube.PodSucceeded {
		// Pod succeeded. Update ProwJob, talk to GitHub, and start next jobs.
		pj.Status.CompletionTime = time.Now()
		pj.Status.State = kube.SuccessState
		pj.Status.Description = "Job succeeded."
		c.reports = append(c.reports, pj)
		for _, nj := range pj.Spec.RunAfterSuccess {
			if _, err := c.kc.CreateProwJob(NewProwJob(nj)); err != nil {
				return fmt.Errorf("error starting next prowjob: %v", err)
			}
		}
	} else if pod.Status.Phase == kube.PodFailed {
		if pod.Status.Reason == kube.Evicted {
			// Pod was evicted. Restart it.
			pj.Status.PodName = ""
			pj.Status.State = kube.PendingState
		} else {
			// Pod failed. Update ProwJob, talk to GitHub.
			pj.Status.CompletionTime = time.Now()
			pj.Status.State = kube.FailureState
			pj.Status.Description = "Job failed."
			c.reports = append(c.reports, pj)
		}
	} else {
		// Pod is running. Do nothing.
		return nil
	}
	_, err := c.kc.ReplaceProwJob(pj.Metadata.Name, pj)
	return err
}

func (c *Controller) startPod(pj kube.ProwJob) (string, string, error) {
	buildID, err := c.getBuildID(c.totURL, pj.Spec.Job)
	if err != nil {
		return "", "", fmt.Errorf("error getting build ID: %v", err)
	}
	spec := pj.Spec.PodSpec
	spec.RestartPolicy = "Never"
	// Keep this synchronized with get_running_build_log in Gubernator!
	podName := fmt.Sprintf("%s-%s", pj.Spec.Job, buildID)
	if len(podName) > 60 {
		podName = podName[len(podName)-60:]
	}

	// Set environment variables in each container in the pod spec. We don't
	// want to update the spec in place, since that will update the ProwJob
	// spec. Instead, create a copy.
	spec.Containers = []kube.Container{}
	for i := range pj.Spec.PodSpec.Containers {
		spec.Containers = append(spec.Containers, pj.Spec.PodSpec.Containers[i])
		spec.Containers[i].Name = fmt.Sprintf("%s-%d", podName, i)
		spec.Containers[i].Env = append(spec.Containers[i].Env,
			kube.EnvVar{
				Name:  "JOB_NAME",
				Value: pj.Spec.Job,
			},
			kube.EnvVar{
				Name:  "BUILD_NUMBER",
				Value: buildID,
			},
		)
		if pj.Spec.Type == kube.PeriodicJob {
			continue
		}
		spec.Containers[i].Env = append(spec.Containers[i].Env,
			kube.EnvVar{
				Name:  "REPO_OWNER",
				Value: pj.Spec.Refs.Org,
			},
			kube.EnvVar{
				Name:  "REPO_NAME",
				Value: pj.Spec.Refs.Repo,
			},
			kube.EnvVar{
				Name:  "PULL_BASE_REF",
				Value: pj.Spec.Refs.BaseRef,
			},
			kube.EnvVar{
				Name:  "PULL_BASE_SHA",
				Value: pj.Spec.Refs.BaseSHA,
			},
			kube.EnvVar{
				Name:  "PULL_REFS",
				Value: pj.Spec.Refs.String(),
			},
		)
		if pj.Spec.Type == kube.PostsubmitJob || pj.Spec.Type == kube.BatchJob {
			continue
		}
		spec.Containers[i].Env = append(spec.Containers[i].Env,
			kube.EnvVar{
				Name:  "PULL_NUMBER",
				Value: strconv.Itoa(pj.Spec.Refs.Pulls[0].Number),
			},
			kube.EnvVar{
				Name:  "PULL_PULL_SHA",
				Value: pj.Spec.Refs.Pulls[0].SHA,
			},
		)
	}
	p := kube.Pod{
		Metadata: kube.ObjectMeta{
			Name: podName,
		},
		Spec: spec,
	}
	actual, err := c.pkc.CreatePod(p)
	if err != nil {
		return "", "", fmt.Errorf("error creating pod: %v", err)
	}
	return buildID, actual.Metadata.Name, nil
}

func (c *Controller) getBuildID(server, name string) (string, error) {
	var err error
	url := server + "/vend/" + name
	for retries := 0; retries < 60; retries++ {
		if retries > 0 {
			time.Sleep(2 * time.Second)
		}
		var resp *http.Response
		resp, err = http.Get(url)
		if err != nil {
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			continue
		}
		if buf, err := ioutil.ReadAll(resp.Body); err == nil {
			return string(buf), nil
		} else {
			return "", err
		}
	}
	return "", err
}

// TODO(spxtr): Template this.
func guberURL(pj kube.ProwJob, build string) string {
	var url string
	if pj.Spec.Type == kube.PresubmitJob || pj.Spec.Type == kube.BatchJob {
		url = guberBasePR
	} else {
		url = guberBasePush
	}
	if pj.Spec.Refs.Org != "kubernetes" {
		url = fmt.Sprintf("%s/%s_%s", url, pj.Spec.Refs.Org, pj.Spec.Refs.Repo)
	} else if pj.Spec.Refs.Repo != "kubernetes" {
		url = fmt.Sprintf("%s/%s", url, pj.Spec.Refs.Repo)
	}
	switch t := pj.Spec.Type; t {
	case kube.PresubmitJob:
		return fmt.Sprintf("%s/%s/%s/%s/", url, strconv.Itoa(pj.Spec.Refs.Pulls[0].Number), pj.Spec.Job, build)
	case kube.PostsubmitJob:
		return fmt.Sprintf("%s/%s/%s/", url, pj.Spec.Job, build)
	case kube.PeriodicJob:
		return fmt.Sprintf("%s/%s/%s/", url, pj.Spec.Job, build)
	case kube.BatchJob:
		return fmt.Sprintf("%s/batch/%s/%s/", url, pj.Spec.Job, build)
	default:
		return testInfra
	}
}
