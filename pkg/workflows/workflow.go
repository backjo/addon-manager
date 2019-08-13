/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package workflows

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/ghodss/yaml"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	addonmgrv1alpha1 "github.com/orkaproj/addon-manager/api/v1alpha1"
	"github.com/orkaproj/addon-manager/pkg/common"
)

// AddonLifecycle represents the following workflows
type AddonLifecycle interface {
	Install(context.Context, *addonmgrv1alpha1.WorkflowType, string) (addonmgrv1alpha1.ApplicationAssemblyPhase, error)
	Delete(string) error
}

type workflowLifecycle struct {
	client.Client
	dynClient dynamic.Interface
	addon     *addonmgrv1alpha1.Addon
	recorder  record.EventRecorder
	scheme    *runtime.Scheme
}

// NewWorkflowLifecycle returns a NewWorkflowLifecycle object
func NewWorkflowLifecycle(client client.Client, dynClient dynamic.Interface, addon *addonmgrv1alpha1.Addon, recorder record.EventRecorder, scheme *runtime.Scheme) AddonLifecycle {
	return &workflowLifecycle{
		Client:    client,
		dynClient: dynClient,
		addon:     addon,
		recorder:  recorder,
		scheme:    scheme,
	}
}

func (w *workflowLifecycle) Install(ctx context.Context, wt *addonmgrv1alpha1.WorkflowType, name string) (addonmgrv1alpha1.ApplicationAssemblyPhase, error) {
	wp := &unstructured.Unstructured{}
	err := w.parse(wt, wp, name)
	if err != nil {
		return addonmgrv1alpha1.Failed, fmt.Errorf("invalid workflow. %v", err)
	}

	if !w.configureGlobalWFParameters(w.addon, wp) {
		return addonmgrv1alpha1.Failed, errors.New("invalid workflow parameter")
	}

	err = w.configureWorkflowArtifacts(wp, wt)
	if err != nil {
		return addonmgrv1alpha1.Failed, err
	}

	return w.submit(ctx, wp)
}

// Appends addon.spec.params to workflow.spec.arguments.parameters
func (w *workflowLifecycle) configureGlobalWFParameters(addon *addonmgrv1alpha1.Addon, wf *unstructured.Unstructured) bool {
	// get workflow argument parameters
	spec, _ := wf.UnstructuredContent()["spec"].(map[string]interface{})
	if spec["arguments"] == nil {
		spec["arguments"] = make(map[string]interface{})
	}

	arguments := spec["arguments"].(map[string]interface{})
	if arguments["parameters"] == nil {
		arguments["parameters"] = make([]interface{}, 0)
	}

	wfParams := arguments["parameters"].([]interface{})
	if wfParams == nil {
		arguments["parameters"] = make([]interface{}, 0)
	}

	// get addon params
	namespaceParam := addon.Spec.Params.Namespace
	contextParams := addon.Spec.Params.Context
	dataParams := addon.Spec.Params.Data

	namespaceMap := make(map[string]interface{})
	namespaceMap["name"] = "namespace"
	namespaceMap["value"] = namespaceParam

	wfParams = append(wfParams, namespaceMap)

	// Copy general Context string params to global workflow variables (clusterName and clusterRegion currently)
	cp := reflect.ValueOf(contextParams)
	for i := 0; i < cp.Type().NumField(); i++ {
		contextMap := make(map[string]interface{})
		kind := cp.Field(i).Kind()
		if kind == reflect.String {
			fieldName := cp.Type().Field(i).Name
			tag := cp.Type().Field(i).Tag
			jsonTag := strings.Split(tag.Get("json"), ",")[0]
			contextMap["name"] = jsonTag
			contextMap["value"] = cp.FieldByName(fieldName).String()
			wfParams = append(wfParams, contextMap)
		}
	}

	// Copy AdditionalConfigs from Context to global workflow variables
	for name, value := range contextParams.AdditionalConfigs {
		addParam := make(map[string]interface{})
		addParam["name"] = name
		addParam["value"] = string(value)
		wfParams = append(wfParams, addParam)
	}

	// Copy stringParams to global workflow variables
	for name, value := range dataParams {
		addParam := make(map[string]interface{})
		addParam["name"] = name
		addParam["value"] = string(value)
		wfParams = append(wfParams, addParam)
	}

	err := unstructured.SetNestedSlice(wf.UnstructuredContent(), wfParams, "spec", "arguments", "parameters")
	if err != nil {
		return false
	}

	return true
}

func (w *workflowLifecycle) Delete(name string) error {
	err := w.dynClient.Resource(common.WorkflowGVR()).Namespace(w.addon.Namespace).Delete(name, &metav1.DeleteOptions{})
	if err != nil {
		return err
	}
	return nil
}

func (w *workflowLifecycle) findWorkflowByName(ctx context.Context, name types.NamespacedName) (*unstructured.Unstructured, error) {
	found := &unstructured.Unstructured{}
	found.SetGroupVersionKind(schema.GroupVersionKind{
		Kind:    "Workflow",
		Group:   "argoproj.io",
		Version: "v1alpha1",
	})
	err := w.Get(ctx, name, found)
	if err != nil && apierrors.IsNotFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	return found, nil
}

func (w *workflowLifecycle) submit(ctx context.Context, wp *unstructured.Unstructured) (addonmgrv1alpha1.ApplicationAssemblyPhase, error) {
	var wfv1 *unstructured.Unstructured

	// Check if the Workflow already exists
	wfv1, err := w.findWorkflowByName(ctx, types.NamespacedName{Name: wp.GetName(), Namespace: wp.GetNamespace()})
	if err != nil {
		return addonmgrv1alpha1.Failed, err
	}

	// Check if the same Addon spec was submitted and completed previously
	if wfv1 != nil {
		deleted, err := w.deleteCollisionWorkflows(wfv1)
		if err != nil {
			return addonmgrv1alpha1.Failed, err
		}
		if deleted {
			return addonmgrv1alpha1.Pending, nil
		}
	}

	if wfv1 == nil {
		// Create the Workflow
		wfv1 := &unstructured.Unstructured{}

		// Convert proxy to workflow object
		err = w.scheme.Convert(wp, wfv1, 0)
		if err != nil {
			return addonmgrv1alpha1.Failed, err
		}
		wfv1.SetGroupVersionKind(schema.GroupVersionKind{
			Kind:    "Workflow",
			Group:   "argoproj.io",
			Version: "v1alpha1",
		})
		wfv1.SetNamespace(wp.GetNamespace())
		wfv1.SetName(wp.GetName())
		// Set the owner references for workflow
		if err := controllerutil.SetControllerReference(w.addon, wfv1, w.scheme); err != nil {
			return addonmgrv1alpha1.Failed, err
		}
		ownerReferences := wfv1.GetOwnerReferences()
		for _, ref := range ownerReferences {
			if strings.ToLower(ref.Kind) == "addon" {
				*ref.Controller = false
			}
		}
		wfv1.SetOwnerReferences(ownerReferences)

		err = w.Create(ctx, wfv1)
		if err != nil {
			return addonmgrv1alpha1.Failed, err
		}
		// Record an event for created workflow
		w.recorder.Event(w.addon, "Normal", "Created", fmt.Sprintf("Created Workflow %s/%s", wp.GetName(), wp.GetNamespace()))

		return addonmgrv1alpha1.Pending, nil
	}

	workflow, err := w.dynClient.Resource(common.WorkflowGVR()).Namespace(wfv1.GetNamespace()).Get(wfv1.GetName(), metav1.GetOptions{})
	if err != nil {
		return addonmgrv1alpha1.Failed, fmt.Errorf("could not find workflow %s/%s. %v", wfv1.GetNamespace(), wfv1.GetName(), err)
	}

	// validate workflow status
	var phase = addonmgrv1alpha1.Pending
	status, ok := workflow.UnstructuredContent()["status"].(map[string]interface{})
	if ok && status["phase"] == "Succeeded" {
		phase = addonmgrv1alpha1.Succeeded
	} else if ok && status["phase"] == "Failed" {
		phase = addonmgrv1alpha1.Failed
	}

	return phase, nil
}

func (w *workflowLifecycle) parse(wt *addonmgrv1alpha1.WorkflowType, wf *unstructured.Unstructured, name string) error {
	var data map[string]interface{}

	// Load workflow spec into data obj
	if err := yaml.Unmarshal([]byte(wt.Template), &data); err != nil {
		return fmt.Errorf("invalid workflow yaml spec passed. %v", err)
	}

	wf.SetGroupVersionKind(schema.GroupVersionKind{
		Kind:    "Workflow",
		Group:   "argoproj.io",
		Version: "v1alpha1",
	})

	wf.SetNamespace(w.addon.GetNamespace())
	wf.SetName(name)
	content := wf.UnstructuredContent()

	spec, ok := data["spec"]
	if !ok {
		return errors.New("invalid workflow, missing spec")
	}

	// Make sure workflows by default get cleaned up after 3 days
	if ttlSecondAfterFinished := spec.(map[string]interface{})["ttlSecondsAfterFinished"]; ttlSecondAfterFinished == nil {
		spec.(map[string]interface{})["ttlSecondsAfterFinished"] = int64(259200)
	}

	content["spec"] = spec
	wf.SetUnstructuredContent(content)

	return nil
}

func (w *workflowLifecycle) configureWorkflowArtifacts(wf *unstructured.Unstructured, wt *addonmgrv1alpha1.WorkflowType) error {
	spec, _, _ := unstructured.NestedFieldNoCopy(wf.UnstructuredContent(), "spec")

	// workflow.spec.arguments.artifacts may exist
	err := w.processArtifacts(spec, wt)
	if err != nil {
		return err
	}

	templates, _, _ := unstructured.NestedFieldNoCopy(wf.UnstructuredContent(), "spec", "templates")
	for _, template := range templates.([]interface{}) {
		if allSteps, found, _ := unstructured.NestedFieldNoCopy(template.(map[string]interface{}), "steps"); found {
			for _, steps := range allSteps.([]interface{}) {
				steps := steps.([]interface{})
				for _, step := range steps {
					err := w.processArtifacts(step, wt)
					if err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

func (w *workflowLifecycle) processArtifacts(workflowStepObject interface{}, wt *addonmgrv1alpha1.WorkflowType) error {
	artifacts, _, _ := unstructured.NestedFieldNoCopy(workflowStepObject.(map[string]interface{}), "arguments", "artifacts")

	if artifacts == nil {
		return nil
	}

	for _, artifact := range artifacts.([]interface{}) {
		artifact := artifact.(map[string]interface{})
		data, _, _ := unstructured.NestedString(artifact, "raw", "data")
		resources := strings.Split(data, "---\n")

		var newData []string
		for _, obj := range resources {
			resource := make(map[string]interface{})
			err := yaml.Unmarshal([]byte(obj), &resource)
			if err != nil {
				return fmt.Errorf("unable to unmarshall artifact: %s", obj)
			}

			kind := resource["kind"].(string)
			if kind == "StatefulSet" || kind == "Deployment" || kind == "DaemonSet" || kind == "ReplicaSet" || kind == "Service" {
				// Add the default labels to the resource yaml
				resource, err := w.addDefaultLabelsToResource(resource)
				if err != nil {
					return err
				}
				// Add the provided role annotation to the resource yaml
				if wt.Role != "" {
					resource, err = w.addRoleAnnotateToResource(resource, wt)
					if err != nil {
						return err
					}
				}
			}
			appendData, err := yaml.Marshal(resource)
			if err != nil {
				return fmt.Errorf("unable to marshall resource: %+v", resource)
			}
			newData = append(newData, string(appendData))
		}
		data = strings.Join(newData, "---\n")
		err := unstructured.SetNestedField(artifact, data, "raw", "data")
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *workflowLifecycle) addDefaultLabelsToResource(resource map[string]interface{}) (map[string]interface{}, error) {
	metadata, _, err := unstructured.NestedMap(resource, "metadata")
	if err != nil {
		return nil, err
	}
	if _, found, _ := unstructured.NestedMap(metadata, "labels"); !found {
		err = unstructured.SetNestedMap(metadata, make(map[string]interface{}), "labels")
		if err != nil {
			return nil, err
		}
	}

	labels, _, _ := unstructured.NestedMap(metadata, "labels")
	packageSpec := w.addon.GetPackageSpec()

	// Set default labels
	labels["app.kubernetes.io/name"] = w.addon.Name
	labels["app.kubernetes.io/version"] = packageSpec.PkgVersion
	labels["app.kubernetes.io/part-of"] = w.addon.Name
	labels["app.kubernetes.io/managed-by"] = common.AddonGVR().Group

	err = unstructured.SetNestedMap(resource, labels, "metadata", "labels")
	if err != nil {
		return nil, err
	}

	return resource, nil
}

func (w *workflowLifecycle) addRoleAnnotateToResource(resource map[string]interface{}, wt *addonmgrv1alpha1.WorkflowType) (map[string]interface{}, error) {
	metadata, found, _ := unstructured.NestedMap(resource, "spec", "template", "metadata")
	if !found {
		err := unstructured.SetNestedMap(resource, make(map[string]interface{}), "spec", "template", "metadata")
		if err != nil {
			return nil, err
		}
		metadata, _, _ = unstructured.NestedMap(resource, "spec", "template", "metadata")
	}

	if _, found, _ := unstructured.NestedMap(metadata, "annotations"); !found {
		err := unstructured.SetNestedMap(metadata, make(map[string]interface{}), "annotations")
		if err != nil {
			return nil, err
		}
	}

	annotations, _, _ := unstructured.NestedMap(metadata, "annotations")

	annotations["iam.amazonaws.com/role"] = wt.Role

	err := unstructured.SetNestedMap(resource, annotations, "spec", "template", "metadata", "annotations")
	if err != nil {
		return nil, err
	}

	return resource, nil
}

func (w *workflowLifecycle) deleteCollisionWorkflows(wfv1 *unstructured.Unstructured) (bool, error) {
	var mostRecentWorkflowTime time.Time
	var mostRecentWorkflow unstructured.Unstructured
	var deleted = false

	workflows, err := w.dynClient.Resource(common.WorkflowGVR()).Namespace(w.addon.GetNamespace()).List(metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to list workflows. %v", err)
	}

	// Get the most recently run workflow for this addon
	for _, workflow := range workflows.Items {
		if strings.Contains(workflow.GetName(), w.addon.Name) {
			if workflow.UnstructuredContent()["status"] == nil {
				return false, nil
			}
			startedAt := workflow.UnstructuredContent()["status"].(map[string]interface{})["startedAt"].(string)
			t, err := time.Parse(time.RFC3339, startedAt)
			if err != nil {
				return false, err
			}
			if !t.Before(mostRecentWorkflowTime) {
				mostRecentWorkflowTime = t
				mostRecentWorkflow = workflow
			}
		}
	}

	if mostRecentWorkflow.Object == nil {
		return false, nil
	}

	// If the most recently run workflow doesn't have the current checksum, delete the old checksum workflows
	if !strings.Contains(mostRecentWorkflow.GetName(), w.addon.Status.Checksum) {
		for _, workflow := range workflows.Items {
			phase := workflow.UnstructuredContent()["status"].(map[string]interface{})["phase"].(string)
			if strings.Contains(workflow.GetName(), w.addon.Status.Checksum) && phase != "Pending" {
				_ = w.Delete(workflow.GetName())
				deleted = true
			}
		}
	}

	return deleted, nil
}
