/*
Copyright 2018, 2019 the Velero contributors.

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

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	velerov1client "github.com/vmware-tanzu/velero/pkg/generated/clientset/versioned/typed/velero/v1"
)

// RestorePlugin is a restore item action plugin for Velero
type RestorePlugin struct {
	logger          logrus.FieldLogger
	configMapClient corev1.ConfigMapInterface
	veleroClient    velerov1client.VeleroV1Interface
}

// NewRestorePlugin instantiates a RestorePlugin.
func NewRestorePlugin(logger logrus.FieldLogger) *RestorePlugin {
	// Kubernetes client
	config, err := rest.InClusterConfig()
	if err != nil {
		logger.Fatalf("Failed to create in-cluster config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		logger.Fatalf("Failed to create clientset: %v", err)
	}
	configMapClient := clientset.CoreV1().ConfigMaps("velero")

	veleroClient, err := velerov1client.NewForConfig(config)
	if err != nil {
		logger.Fatalf("Failed to create Velero client: %v", err)
	}

	return &RestorePlugin{
		logger:          logger,
		configMapClient: configMapClient,
		veleroClient:    veleroClient,
	}
}

// AppliesTo returns a ResourceSelector that matches all resources
func (p *RestorePlugin) AppliesTo() (velero.ResourceSelector, error) {
	return velero.ResourceSelector{}, nil
}

// Execute allows the RestorePlugin to perform arbitrary logic with the item being restored
func (p *RestorePlugin) Execute(input *velero.RestoreItemActionExecuteInput) (*velero.RestoreItemActionExecuteOutput, error) {
	p.logger.Info("Executing CustomRestorePlugin")
	defer p.logger.Info("Done executing CustomRestorePlugin")

	// Fetch patterns from ConfigMaps based on label selector
	patterns, err := p.getConfigMapDataByLabel("agoracalyce.io/replace-pattern=RestoreItemAction")
	if err != nil {
		p.logger.Warnf("No ConfigMap found or error fetching ConfigMap: %v", err)
		return velero.NewRestoreItemActionExecuteOutput(input.Item), nil // Continue without applying the plugin logic if ConfigMap is not found
	}

	output, err := replacePatternAction(p, input, patterns)
	if err != nil {
		return nil, err
	}

	// Trigger podvolumerestore manually
	modifiedItem, ok := output.UpdatedItem.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("failed to assert type to *unstructured.Unstructured")
	}
	if err := p.triggerPodVolumeRestore(modifiedItem); err != nil {
		p.logger.Warnf("Failed to trigger podvolumerestore: %v", err)
	}

	return output, nil
}

func (p *RestorePlugin) getConfigMapDataByLabel(labelSelector string) (map[string]string, error) {
	configMaps, err := p.configMapClient.List(context.TODO(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list configmaps: %v", err)
	}

	if len(configMaps.Items) == 0 {
		return nil, fmt.Errorf("no configmap found with label selector: %s", labelSelector)
	}

	// So we can use this plugin simultaneously
	aggregatedPatterns := make(map[string]string)
	for _, configMap := range configMaps.Items {
		for key, value := range configMap.Data {
			aggregatedPatterns[key] = value
		}
	}

	return aggregatedPatterns, nil
}

func replacePatternAction(p *RestorePlugin, input *velero.RestoreItemActionExecuteInput, patterns map[string]string) (*velero.RestoreItemActionExecuteOutput, error) {
	p.logger.Infof("Executing ReplacePatternAction on %v", input.Item.GetObjectKind().GroupVersionKind().Kind)

	jsonData, err := json.Marshal(input.Item)
	if err != nil {
		return nil, err
	}

	modifiedString := string(jsonData)
	var originalName string

	if input.Item.GetObjectKind().GroupVersionKind().Kind == "Pod" {
		originalName = extractMetadataName(jsonData)
	}

	for pattern, replacement := range patterns {
		modifiedString = strings.ReplaceAll(modifiedString, pattern, replacement)
	}

	if input.Item.GetObjectKind().GroupVersionKind().Kind == "Pod" {
		modifiedString = restoreMetadataName(modifiedString, originalName)
	}

	// Create a new item from the modified JSON data
	var modifiedObj unstructured.Unstructured
	if err := json.Unmarshal([]byte(modifiedString), &modifiedObj); err != nil {
		return nil, err
	}
	return velero.NewRestoreItemActionExecuteOutput(&modifiedObj), nil
}

func extractMetadataName(jsonData []byte) string {
	var obj map[string]interface{}
	if err := json.Unmarshal(jsonData, &obj); err != nil {
		return ""
	}

	if metadata, ok := obj["metadata"].(map[string]interface{}); ok {
		if name, ok := metadata["name"].(string); ok {
			return name
		}
	}
	return ""
}

func restoreMetadataName(modifiedString, originalName string) string {
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(modifiedString), &obj); err != nil {
		return modifiedString
	}

	if metadata, ok := obj["metadata"].(map[string]interface{}); ok {
		metadata["name"] = originalName
	}

	result, err := json.Marshal(obj)
	if err != nil {
		return modifiedString
	}
	return string(result)
}

func (p *RestorePlugin) triggerPodVolumeRestore(modifiedItem *unstructured.Unstructured) error {
	veleroNamespace := "velero"
	// Check if the resource is a Pod and trigger podvolumerestore logic
	if modifiedItem.GetKind() == "Pod" {
		name := modifiedItem.GetName()
		labels := modifiedItem.GetLabels()
		if labels == nil {
			return fmt.Errorf("pod labels are nil")
		}

		restoreName, restoreUID := labels["velero.io/restore-name"], labels["velero.io/restore-uid"]
		if restoreName == "" || restoreUID == "" {
			return fmt.Errorf("missing restore-name or restore-uid in pod labels")
		}

		pvrList, err := p.veleroClient.PodVolumeRestores(veleroNamespace).List(context.TODO(), metav1.ListOptions{
			LabelSelector: fmt.Sprintf("velero.io/restore-name=%s,velero.io/restore-uid=%s", restoreName, restoreUID),
		})
		if err != nil {
			return fmt.Errorf("failed to list PodVolumeRestores: %v", err)
		}

		for _, pvr := range pvrList.Items {
			if pvr.Spec.Pod.Name == name {
				pvrCopy := pvr.DeepCopy()
				pvrCopy.Status.Phase = velerov1api.PodVolumeRestorePhaseInProgress
				_, err := p.veleroClient.PodVolumeRestores(veleroNamespace).UpdateStatus(context.TODO(), pvrCopy, metav1.UpdateOptions{})
				if err != nil {
					return fmt.Errorf("failed to update PodVolumeRestore status: %v", err)
				}
			}
		}
	}
	return nil
}
