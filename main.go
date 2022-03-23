package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	flag "github.com/spf13/pflag"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"

	_ "k8s.io/client-go/plugin/pkg/client/auth"
)

var verbose bool
var exitErr bool
var sockFound bool

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v", err)
		os.Exit(1)
	}
	if exitErr && sockFound {
		os.Exit(1)
	} else {
		os.Exit(0)
	}
}

func run() error {
	// flags
	requestedNamespace := flag.StringP("namespace", "n", "ALL", "Namespace to search for pods")
	requestedFile := flag.StringP("filename", "f", "", "File or directory to scan")
	help := flag.BoolP("help", "h", false, "Print usage")
	flag.BoolVarP(&exitErr, "exit-with-error", "e", false, "Exit with error code if docker.sock found")
	flag.BoolVarP(&verbose, "verbose", "v", false, "Enable verbose logging")

	flag.Parse()

	if *help {
		flag.PrintDefaults()
		os.Exit(0)
	}

	// initialize tabwriter
	w := new(tabwriter.Writer)

	// minwidth, tabwidth, padding, padchar, flags
	w.Init(os.Stdout, 8, 8, 0, '\t', 0)

	defer w.Flush()

	// only scan local files if -f is provided
	if len(*requestedFile) > 0 {
		var files []string
		fmt.Fprintf(w, "%s\t%s\t%s\t\n", "FILE", "LINE", "STATUS")

		fileInfo, err := os.Stat(*requestedFile)
		if err != nil {
			return fmt.Errorf("unable to open file: %v\n", *requestedFile)
		}

		if fileInfo.IsDir() {
			err = filepath.Walk(*requestedFile, func(path string, info os.FileInfo, err error) error {
				pathInfo, err := os.Stat(path)
				if !pathInfo.IsDir() {
					files = append(files, path)
				}
				return nil
			})
			if err != nil {
				fmt.Printf("something went wrong")
				return err
			}

		} else {
			// filePath is a regular file
			files = append(files, *requestedFile)
		}

		printFiles(w, files)

		if exitErr && sockFound {
			os.Exit(1)
		} else {
			os.Exit(0)
		}
	}

	// setup kubeconfig client
	configFlags := genericclioptions.NewConfigFlags(true).WithDiscoveryBurst(300).WithDiscoveryQPS(50.0)
	kubeConfig, err := configFlags.ToRESTConfig()
	if err != nil {
		return fmt.Errorf("error loading kubeconfig: %v", err)
	}
	clientset := kubernetes.NewForConfigOrDie(kubeConfig)

	// Column headers for live cluster scan
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\t\n", "NAMESPACE", "TYPE", "NAME", "STATUS")

	if *requestedNamespace != "ALL" {
		if verbose {
			fmt.Printf("user specified namespace: %s\n", *requestedNamespace)
		}
		namespace, err := clientset.CoreV1().Namespaces().Get(context.Background(), *requestedNamespace, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("unable to fetch namespace %q: %v", *requestedNamespace, err)
		}
		return printResources(*namespace, clientset, w)
	} else {
		namespaceList, err := clientset.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("unable to list namespaces: %v", err)
		}

		namespaceErrors := make([]error, 0)
		// loop through each namespace
		for _, namespace := range namespaceList.Items {
			if err := printResources(namespace, clientset, w); err != nil {
				namespaceErrors = append(namespaceErrors, err)
			}
		}
		if len(namespaceErrors) > 0 {
			return utilerrors.NewAggregate(namespaceErrors)
		}
	}
	return nil
}

func printResources(namespace corev1.Namespace, clientset *kubernetes.Clientset, w *tabwriter.Writer) error {
	namespaceName := namespace.ObjectMeta.Name

	nsDeployments := make(map[string]*appsv1.Deployment)
	nsDaemonsets := make(map[string]*appsv1.DaemonSet)
	nsStatefulsets := make(map[string]*appsv1.StatefulSet)
	nsJobs := make(map[string]*batchv1.Job)
	nsCronJobs := make(map[string]*batchv1.CronJob)

	// Get a list of all pods in the namespace
	podList, err := clientset.CoreV1().Pods(namespaceName).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("unable to fetch pods: %v", err)
	}

	errorList := make([]error, 0)
	// loop through each pod
	for _, p := range podList.Items {
		// print object
		// fmt.Printf("%+v\n", p.ObjectMeta.OwnerReferences)

		// only look at pods that have volumes
		if len(p.Spec.Volumes) != 0 {
			// fmt.Printf("%T\n", p.ObjectMeta.OwnerReferences)

			// true if pod has an owner (eg deployment, daemonset, job)
			if len(p.ObjectMeta.OwnerReferences) != 0 {
				podOwner := p.OwnerReferences[0].Name

				// Supported owner types are
				// ReplicaSet (looks up deployment)
				// DaemonSet
				// StatefulSet
				// ...
				switch p.ObjectMeta.OwnerReferences[0].Kind {
				case "ReplicaSet":
					replica, rsErr := clientset.AppsV1().ReplicaSets(namespace.Name).Get(context.TODO(), podOwner, metav1.GetOptions{})
					if rsErr != nil {
						errorList = append(errorList, rsErr)
						continue
					}

					deployment, deployErr := clientset.AppsV1().Deployments(namespace.Name).Get(context.TODO(), replica.OwnerReferences[0].Name, metav1.GetOptions{})
					if deployErr != nil {
						errorList = append(errorList, deployErr)
						continue
					}

					// append the current deployment to look up later
					// only append if it's not already in the list
					if _, ok := nsDeployments[deployment.Name]; !ok {
						nsDeployments[deployment.Name] = deployment
					}
				case "DaemonSet":
					daemonset, dsErr := clientset.AppsV1().DaemonSets(namespace.Name).Get(context.TODO(), podOwner, metav1.GetOptions{})
					if dsErr != nil {
						errorList = append(errorList, dsErr)
						continue
					}

					// append the current daemonset to look up later
					if _, ok := nsDaemonsets[daemonset.Name]; !ok {
						nsDaemonsets[daemonset.Name] = daemonset
					}
				case "StatefulSet":
					statefulset, ssErr := clientset.AppsV1().StatefulSets(namespace.Name).Get(context.TODO(), podOwner, metav1.GetOptions{})
					if ssErr != nil {
						errorList = append(errorList, ssErr)
						continue
					}

					// append the current StatefulSet to look up later
					if _, ok := nsStatefulsets[statefulset.Name]; !ok {
						nsStatefulsets[statefulset.Name] = statefulset
					}
				case "Node":
					// skip pods with owner type node because they're static pods
					continue
				case "Job":
					job, jobErr := clientset.BatchV1().Jobs(namespace.Name).Get(context.TODO(), podOwner, metav1.GetOptions{})
					if jobErr != nil {
						errorList = append(errorList, jobErr)
						continue
					}

					// check if the job has an owner
					// If it does then it's part of a CronJob
					if len(job.ObjectMeta.OwnerReferences) == 0 {
						if _, ok := nsJobs[job.Name]; !ok {
							nsJobs[job.Name] = job
						}
					} else {
						// append to cronjob
						cron, cronErr := clientset.BatchV1().CronJobs(namespace.Name).Get(context.TODO(), job.OwnerReferences[0].Name, metav1.GetOptions{})
						if cronErr != nil {
							errorList = append(errorList, cronErr)
							continue
						}

						if _, ok := nsCronJobs[cron.Name]; !ok {
							nsCronJobs[cron.Name] = cron
						}
					}

				default:
					// this prints for pods that say they have an owner but the owner doesn't exist
					// happens with vcluster clusters and maybe other situations.
					fmt.Printf("could not find resource manager for type %s for pod %s\n", p.OwnerReferences[0].Kind, p.Name)
					continue
				}
			} else {
				// Look up raw pods for volumes here
				printVolumes(w, p.Spec.Volumes, namespaceName, "pod", p.Name)
			}
		}
	}
	// loop through all the unique deployments we found for volumes
	for _, deploy := range nsDeployments {
		printVolumes(w, deploy.Spec.Template.Spec.Volumes, namespaceName, "deployment", deploy.Name)
	}

	// loop through all the unique DaemonSets in the namespace
	for _, daemonset := range nsDaemonsets {
		volumeCounter := 0
		for _, v := range daemonset.Spec.Template.Spec.Volumes {
			if v.VolumeSource.HostPath != nil {
				// fmt.Printf("testing %s\n", v.VolumeSource.HostPath.Path)
				if strings.Contains(v.VolumeSource.HostPath.Path, "docker.sock") {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t\n", namespaceName, "daemonset", daemonset.Name, "mounted")
					break
				}
			}
			volumeCounter++
			if volumeCounter == len(daemonset.Spec.Template.Spec.Volumes) && verbose {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t\n", namespaceName, "daemonset", daemonset.Name, "not-mounted")
			}
		}
	}

	// loop through all the unique StatefulSets in the namespace
	for _, statefulset := range nsStatefulsets {
		printVolumes(w, statefulset.Spec.Template.Spec.Volumes, namespaceName, "statefulset", statefulset.Name)
	}

	// loop through all the unique Jobs in the namespace
	for _, job := range nsJobs {
		printVolumes(w, job.Spec.Template.Spec.Volumes, namespaceName, "job", job.Name)
	}

	// loop through all the unique CronJobs in the namespace
	for _, cron := range nsCronJobs {
		printVolumes(w, cron.Spec.JobTemplate.Spec.Template.Spec.Volumes, namespaceName, "cron", cron.Name)
	}

	if len(errorList) > 0 {
		return utilerrors.NewAggregate(errorList)
	}
	return nil
}

func printVolumes(w *tabwriter.Writer, volumes []corev1.Volume, namespace, resType, resName string) {
	for _, v := range volumes {
		if v.VolumeSource.HostPath != nil {
			mounted := "not-mounted"
			if strings.Contains(v.VolumeSource.HostPath.Path, "docker.sock") {
				mounted = "mounted"
				sockFound = true
			}
			if mounted == "mounted" || verbose {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t\n", namespace, resType, resName, mounted)
			}
		}
	}
}

func printFiles(w *tabwriter.Writer, filePaths []string) {
	for _, file := range filePaths {
		mounted := "not-mounted"
		line, err := searchFile(file)
		if err != nil {
			return
		}
		if line > 0 {
			mounted = "mounted"
			sockFound = true
		}
		if mounted == "mounted" || verbose {
			fmt.Fprintf(w, "%s\t%v\t%s\t\n", file, line, mounted)
		}
	}
}

func searchFile(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// Splits on newlines by default.
	scanner := bufio.NewScanner(f)

	line := 1
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "docker.sock") {
			return line, nil
		}

		line++
	}
	return 0, nil
}
