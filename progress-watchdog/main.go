package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/op/go-logging"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var log = logging.MustGetLogger("ProgressWatchdog")

var format = logging.MustStringFormatter(
	`%{time:15:04:05.000} %{shortfunc}: %{level:.4s} %{message}`,
)

// ContinueCodePayload json format of the get continue code response
type ContinueCodePayload struct {
	ContinueCode string `json:"continueCode"`
}

// ProgressUpdateJobs contains all information required by a ProgressUpdateJobs worker to do its Job
type ProgressUpdateJobs struct {
	Teamname         string
	LastContinueCode string
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}

func main() {
	logBackend := logging.NewLogBackend(os.Stdout, "", 0)

	logFormatter := logging.NewBackendFormatter(logBackend, format)
	logBackendLeveled := logging.AddModuleLevel(logBackend)
	logBackendLeveled.SetLevel(logging.DEBUG, "")

	log.SetBackend(logBackendLeveled)
	logging.SetBackend(logBackendLeveled, logFormatter)

	// config, err := rest.InClusterConfig()
	// if err != nil {
	// 	panic(err.Error())
	// }

	var kubeconfig *string
	if home := homeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err.Error())
	}

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	progressUpdateJobs := make(chan ProgressUpdateJobs)

	for i := 0; i < 10; i++ {
		go workOnProgressUpdates(progressUpdateJobs, clientset)
	}

	createProgressUpdateJobs(progressUpdateJobs, clientset)
}

// Constantly lists all JuiceShops in managed by MultiJuicer and queues progressUpdatesJobs for them
func createProgressUpdateJobs(progressUpdateJobs chan<- ProgressUpdateJobs, clientset *kubernetes.Clientset) {
	for {
		// Get Instances
		log.Debug("Looking for Instances")
		opts := metav1.ListOptions{
			LabelSelector: "app=juice-shop",
		}

		juiceShops, err := clientset.AppsV1().Deployments("default").List(opts)
		if err != nil {
			panic(err.Error())
		}

		log.Debugf("Found %d JuiceShop running", len(juiceShops.Items))

		for _, instance := range juiceShops.Items {
			teamname := instance.Labels["team"]

			if instance.Status.ReadyReplicas != 1 {
				continue
			}

			log.Debugf("Found instance for team %s", teamname)

			progressUpdateJobs <- ProgressUpdateJobs{
				Teamname:         instance.Labels["team"],
				LastContinueCode: instance.Annotations["multi-juicer.iteratec.dev/continueCode"],
			}
		}
		time.Sleep(5 * time.Second)
	}
}

func workOnProgressUpdates(progressUpdateJobs <-chan ProgressUpdateJobs, clientset *kubernetes.Clientset) {
	for job := range progressUpdateJobs {
		log.Debugf("Running ProgressUpdateJob for team '%s'", job.Teamname)
		log.Debug("Fetching cached continue code")
		lastContinueCode := job.LastContinueCode
		log.Debug("Fetching current continue code")
		currentContinueCode := getCurrentContinueCode(job.Teamname)

		if lastContinueCode == "" && currentContinueCode == nil {
			log.Warning("Failed to fetch both current and cached continue code")
		} else if lastContinueCode == "" && currentContinueCode != nil {
			log.Debug("Did not find a cached continue code.")
			log.Debug("Last continue code was nil. This should only happen once per team.")
			cacheContinueCode(clientset, job.Teamname, *currentContinueCode)
		} else if currentContinueCode == nil {
			log.Debug("Could not get current continue code. Juice Shop might be down. Sleeping and retrying in 5 sec")
		} else {
			log.Debug("Checking Difference between continue code")
			if lastContinueCode != *currentContinueCode {
				log.Debugf("Continue codes differ (last vs current): (%s vs %s)", lastContinueCode, *currentContinueCode)
				log.Debug("Applying cached continue code")
				log.Infof("Found new continue Code for Team '%s'", job.Teamname)
				applyContinueCode(job.Teamname, lastContinueCode)
				log.Debug("ReFetching current continue code")
				currentContinueCode = getCurrentContinueCode(job.Teamname)

				log.Debug("Caching current continue code")
				cacheContinueCode(clientset, job.Teamname, *currentContinueCode)
			} else {
				log.Debug("Continue codes are identical. Sleeping")
			}
		}
	}
}

func getCurrentContinueCode(teamname string) *string {
	url := fmt.Sprintf("http://t-%s-juiceshop:3000/rest/continue-code", teamname)

	req, err := http.NewRequest("GET", url, bytes.NewBuffer([]byte{}))
	if err != nil {
		log.Warning("Failed to create http request")
		log.Warning(err)
		return nil
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Warning("Failed to fetch continue code from juice shop.")
		log.Warning(err)
		return nil
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case 200:
		body, err := ioutil.ReadAll(res.Body)

		if err != nil {
			log.Error("Failed to read response body stream.")
			return nil
		}

		continueCodePayload := ContinueCodePayload{}

		err = json.Unmarshal(body, &continueCodePayload)

		if err != nil {
			log.Error("Failed to parse json of a challenge status.")
			log.Error(err)
			return nil
		}

		log.Debugf("Got current continue code: '%s'", continueCodePayload.ContinueCode)

		return &continueCodePayload.ContinueCode
	default:
		log.Warningf("Unexpected response status code '%d'", res.StatusCode)
		return nil
	}
}

func applyContinueCode(teamname, continueCode string) {
	url := fmt.Sprintf("http://t-%s-juiceshop:3000/rest/continue-code/apply/%s", teamname, continueCode)

	req, err := http.NewRequest("PUT", url, bytes.NewBuffer([]byte{}))
	if err != nil {
		log.Warning("Failed to create http request to set the current continue code")
		log.Warning(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Warning("Failed to set the current continue code to juice shop.")
		log.Warning(err)
	}
	defer res.Body.Close()
}

type UpdateProgressDeploymentDiff struct {
	Metadata UpdateProgressDeploymentMetadata `json:"metadata"`
}

type UpdateProgressDeploymentMetadata struct {
	Annotations UpdateProgressDeploymentDiffAnnotations `json:"annotations"`
}

type UpdateProgressDeploymentDiffAnnotations struct {
	ContinueCode     string `json:"multi-juicer.iteratec.dev/continueCode"`
	ChallengesSolved string `json:"multi-juicer.iteratec.dev/challengesSolved"`
}

func cacheContinueCode(clientset *kubernetes.Clientset, teamname string, continueCode string) {
	log.Infof("Updating continue-code of team '%s' to '%s'", teamname, continueCode)

	diff := UpdateProgressDeploymentDiff{
		Metadata: UpdateProgressDeploymentMetadata{
			Annotations: UpdateProgressDeploymentDiffAnnotations{
				ContinueCode:     continueCode,
				ChallengesSolved: "42",
			},
		},
	}

	jsonBytes, err := json.Marshal(diff)
	if err != nil {
		panic("could not encode json")
	}
	log.Debug("Json patch")
	log.Debug(string(jsonBytes))

	_, err = clientset.AppsV1().Deployments("default").Patch(fmt.Sprintf("t-%s-juiceshop", teamname), types.MergePatchType, jsonBytes)
	if err != nil {
		log.Error(err)
		panic("could not patch deployment")
	}
}
