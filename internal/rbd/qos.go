package rbd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// keys for QoS parameters.
	maxReadIOPS            = "MaxReadIOPS"
	minReadIOPS            = "MinReadIOPS"
	maxReadBytesPerSecond  = "MaxReadBytesPerSecond"
	maxWriteBytesPerSecond = "MaxWriteBytesPerSecond"

	// path where the cgroupv2 slice files are located for the pods.
	kubePodsSlicePath = "/sys/fs/cgroup/kubepods.slice"

	// keys to retrieve pod details from volume context.
	csiParameterPrefix = "csi.storage.k8s.io/"
	podNameKey         = csiParameterPrefix + "pod.name"
	podNamespaceKey    = csiParameterPrefix + "pod.namespace"
)

// getIOQoSParameters to get the cgroupv2 IO QoS parameters from a map.
func getIOQoSParameters(parameters map[string]string) string {
	// Extract the relevant parameters from the map
	riops := parameters[maxReadIOPS]
	wiops := parameters[minReadIOPS]
	rbps := parameters[maxReadBytesPerSecond]
	wbps := parameters[maxWriteBytesPerSecond]

	ioQoSString := ""
	// 8:16 rbps=2097152 wbps=max riops=max wiops=120
	if rbps != "" {
		ioQoSString += fmt.Sprintf("rbps=%s ", riops)
	}
	if wbps != "" {
		ioQoSString += fmt.Sprintf("wbps=%s ", wiops)
	}
	if riops != "" {
		ioQoSString += fmt.Sprintf("riops=%s ", riops)
	}
	if wiops != "" {
		ioQoSString += fmt.Sprintf("wiops=%s ", wiops)
	}

	return ioQoSString
}

// getMajAndMinofDevice returns the major and minor device numbers of the
// device file.
func getMajAndMinofDevice(devicePath string) (uint32, uint32, error) {
	var stat syscall.Stat_t
	err := syscall.Stat(devicePath, &stat)
	if err != nil {
		return 0, 0, err
	}

	// Extract major and minor device numbers
	return unix.Major(stat.Rdev), unix.Minor(stat.Rdev), nil
}

// getPod returns the pod details from the volume context.
func getPod(ctx context.Context, client *kubernetes.Clientset, volumeContext map[string]string) (*v1.Pod, error) {
	podName, ok := volumeContext[podNameKey]
	if !ok {
		return nil, fmt.Errorf("pod.name not found in volumeContext")
	}
	podNamespace, ok := volumeContext[podNamespaceKey]
	if !ok {
		return nil, fmt.Errorf("pod.namespace not found in volumeContext")
	}
	pod, err := client.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	return pod, nil
}

// Function to retrieve all slice folder paths for all containers in the pod.
func retrieveContainersSliceFolderPath(podSlicePath string) ([]string, error) {
	var sliceFoldersPath []string

	// Walk the directory and find all slice files
	err := filepath.WalkDir(podSlicePath, func(path string, info os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// If the path is a directory and ends with .slice, append it to the
		// sliceFoldersPath and skip the podSlicePath as it is not a container slice
		if info.IsDir() && strings.HasSuffix(info.Name(), ".slice") && !strings.EqualFold(path, podSlicePath) {
			sliceFoldersPath = append(sliceFoldersPath, path)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return sliceFoldersPath, nil
}

func getPodSlicePath(pod *v1.Pod) (string, error) {
	if pod.Status.QOSClass == "" {
		return "", fmt.Errorf("pod %s/%s does not have a QoS class", pod.Namespace, pod.Name)
	}
	qosClass := pod.Status.QOSClass
	slicePath := ""
	switch qosClass {
	case v1.PodQOSBurstable:
		slicePath = fmt.Sprintf("%s/kubepods-burstable.slice", kubePodsSlicePath)
	case v1.PodQOSBestEffort:
		slicePath = fmt.Sprintf("%s/kubepods-besteffort.slice", kubePodsSlicePath)
	case v1.PodQOSGuaranteed:
		slicePath = kubePodsSlicePath
	}

	if slicePath == "" {
		return "", fmt.Errorf("%s is not supported QoS class", qosClass)
	}

	return slicePath, nil
}
