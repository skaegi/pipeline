/*
Copyright 2019 The Tekton Authors

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

package v1beta1

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tektoncd/pipeline/pkg/apis/validate"
	"github.com/tektoncd/pipeline/pkg/jsonpath"
	"github.com/tektoncd/pipeline/pkg/substitution"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"knative.dev/pkg/apis"
)

var _ apis.Validatable = (*Task)(nil)

func (t *Task) Validate(ctx context.Context) *apis.FieldError {
	if err := validate.ObjectMetadata(t.GetObjectMeta()); err != nil {
		return err.ViaField("metadata")
	}
	return t.Spec.Validate(ctx)
}

func (ts *TaskSpec) Validate(ctx context.Context) *apis.FieldError {

	if len(ts.Steps) == 0 {
		return apis.ErrMissingField("steps")
	}

	if err := validateExpansion(ts); err != nil {
		return err
		// TODO @skaegi
		// for now we'll just output the err -- to validate the same errors are being returned by the old param validation
		// The FieldError formatting is different in the validation test cases
		//fmt.Println(err.Error())
	}

	if err := ValidateVolumes(ts.Volumes).ViaField("volumes"); err != nil {
		return err
	}
	if err := ValidateDeclaredWorkspaces(ts.Workspaces, ts.Steps, ts.StepTemplate); err != nil {
		return err
	}
	mergedSteps, err := MergeStepsWithStepTemplate(ts.StepTemplate, ts.Steps)
	if err != nil {
		return &apis.FieldError{
			Message: fmt.Sprintf("error merging step template and steps: %s", err),
			Paths:   []string{"stepTemplate"},
		}
	}

	if err := validateSteps(mergedSteps).ViaField("steps"); err != nil {
		return err
	}

	// Validate Resources declaration
	if err := ts.Resources.Validate(ctx); err != nil {
		return err
	}

	// Validate that the parameters type are correct
	if err := ValidateParameterTypes(ts.Params); err != nil {
		return err
	}

	// Validate task step names
	for _, step := range ts.Steps {
		if errs := validation.IsDNS1123Label(step.Name); step.Name != "" && len(errs) > 0 {
			return &apis.FieldError{
				Message: fmt.Sprintf("invalid value %q", step.Name),
				Paths:   []string{"taskspec.steps.name"},
				Details: "Task step name must be a valid DNS Label, For more info refer to https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names",
			}
		}
	}

	if err := ValidateParameterVariables(ts.Steps, ts.Params); err != nil {
		return err
	}

	if err := ValidateResourcesVariables(ts.Steps, ts.Resources); err != nil {
		return err
	}

	if err := ValidateResults(ts.Results); err != nil {
		return err
	}
	return nil
}

func ValidateResults(results []TaskResult) *apis.FieldError {
	for index, result := range results {
		if !resultNameFormatRegex.MatchString(result.Name) {
			return apis.ErrInvalidKeyName(result.Name, fmt.Sprintf("results[%d].name", index), fmt.Sprintf("Name must consist of alphanumeric characters, '-', '_', and must start and end with an alphanumeric character (e.g. 'MyName',  or 'my-name',  or 'my_name', regex used for validation is '%s')", ResultNameFormat))
		}
	}

	return nil
}

// a mount path which conflicts with any other declared workspaces, with the explicitly
// declared volume mounts, or with the stepTemplate. The names must also be unique.
func ValidateDeclaredWorkspaces(workspaces []WorkspaceDeclaration, steps []Step, stepTemplate *corev1.Container) *apis.FieldError {
	mountPaths := sets.NewString()
	for _, step := range steps {
		for _, vm := range step.VolumeMounts {
			mountPaths.Insert(filepath.Clean(vm.MountPath))
		}
	}
	if stepTemplate != nil {
		for _, vm := range stepTemplate.VolumeMounts {
			mountPaths.Insert(filepath.Clean(vm.MountPath))
		}
	}

	wsNames := sets.NewString()
	for _, w := range workspaces {
		// Workspace names must be unique
		if wsNames.Has(w.Name) {
			return &apis.FieldError{
				Message: fmt.Sprintf("workspace name %q must be unique", w.Name),
				Paths:   []string{"workspaces.name"},
			}
		}
		wsNames.Insert(w.Name)
		// Workspaces must not try to use mount paths that are already used
		mountPath := filepath.Clean(w.GetMountPath())
		if _, ok := mountPaths[mountPath]; ok {
			return &apis.FieldError{
				Message: fmt.Sprintf("workspace mount path %q must be unique", mountPath),
				Paths:   []string{"workspaces.mountpath"},
			}
		}
		mountPaths[mountPath] = struct{}{}
	}
	return nil
}

func ValidateVolumes(volumes []corev1.Volume) *apis.FieldError {
	// Task must not have duplicate volume names.
	vols := sets.NewString()
	for _, v := range volumes {
		if vols.Has(v.Name) {
			return &apis.FieldError{
				Message: fmt.Sprintf("multiple volumes with same name %q", v.Name),
				Paths:   []string{"name"},
			}
		}
		vols.Insert(v.Name)
	}
	return nil
}

func validateSteps(steps []Step) *apis.FieldError {
	// Task must not have duplicate step names.
	names := sets.NewString()
	for idx, s := range steps {
		if s.Image == "" {
			return apis.ErrMissingField("Image")
		}

		if s.Script != "" {
			if len(s.Command) > 0 {
				return &apis.FieldError{
					Message: fmt.Sprintf("step %d script cannot be used with command", idx),
					Paths:   []string{"script"},
				}
			}
		}

		if s.Name != "" {
			if names.Has(s.Name) {
				return apis.ErrInvalidValue(s.Name, "name")
			}
			names.Insert(s.Name)
		}

		for _, vm := range s.VolumeMounts {
			if strings.HasPrefix(vm.MountPath, "/tekton/") &&
				!strings.HasPrefix(vm.MountPath, "/tekton/home") {
				return &apis.FieldError{
					Message: fmt.Sprintf("step %d volumeMount cannot be mounted under /tekton/ (volumeMount %q mounted at %q)", idx, vm.Name, vm.MountPath),
					Paths:   []string{"volumeMounts.mountPath"},
				}
			}
			if strings.HasPrefix(vm.Name, "tekton-internal-") {
				return &apis.FieldError{
					Message: fmt.Sprintf(`step %d volumeMount name %q cannot start with "tekton-internal-"`, idx, vm.Name),
					Paths:   []string{"volumeMounts.name"},
				}
			}
		}
	}
	return nil
}

func ValidateParameterTypes(params []ParamSpec) *apis.FieldError {
	for _, p := range params {
		// Ensure param has a valid type.
		validType := false
		for _, allowedType := range AllParamTypes {
			if p.Type == allowedType {
				validType = true
			}
		}
		if !validType {
			return apis.ErrInvalidValue(p.Type, fmt.Sprintf("taskspec.params.%s.type", p.Name))
		}

		// If a default value is provided, ensure its type matches param's declared type.
		if (p.Default != nil) && (p.Default.Type != p.Type) {
			return &apis.FieldError{
				Message: fmt.Sprintf(
					"\"%v\" type does not match default value's type: \"%v\"", p.Type, p.Default.Type),
				Paths: []string{
					fmt.Sprintf("taskspec.params.%s.type", p.Name),
					fmt.Sprintf("taskspec.params.%s.default.type", p.Name),
				},
			}
		}
	}
	return nil
}

func ValidateParameterVariables(steps []Step, params []ParamSpec) *apis.FieldError {
	parameterNames := sets.NewString()
	arrayParameterNames := sets.NewString()

	for _, p := range params {
		parameterNames.Insert(p.Name)
		if p.Type == ParamTypeArray {
			arrayParameterNames.Insert(p.Name)
		}
	}

	if err := validateVariables(steps, "params", parameterNames); err != nil {
		return err
	}
	return validateArrayUsage(steps, "params", arrayParameterNames)
}

func ValidateResourcesVariables(steps []Step, resources *TaskResources) *apis.FieldError {
	if resources == nil {
		return nil
	}
	resourceNames := sets.NewString()
	if resources.Inputs != nil {
		for _, r := range resources.Inputs {
			resourceNames.Insert(r.Name)
		}
	}
	if resources.Outputs != nil {
		for _, r := range resources.Outputs {
			resourceNames.Insert(r.Name)
		}
	}
	return validateVariables(steps, "resources.(?:inputs|outputs)", resourceNames)
}

func validateArrayUsage(steps []Step, prefix string, vars sets.String) *apis.FieldError {
	for _, step := range steps {
		if err := validateTaskNoArrayReferenced("name", step.Name, prefix, vars); err != nil {
			return err
		}
		if err := validateTaskNoArrayReferenced("image", step.Image, prefix, vars); err != nil {
			return err
		}
		if err := validateTaskNoArrayReferenced("workingDir", step.WorkingDir, prefix, vars); err != nil {
			return err
		}
		if err := validateTaskNoArrayReferenced("script", step.Script, prefix, vars); err != nil {
			return err
		}
		for i, cmd := range step.Command {
			if err := validateTaskArraysIsolated(fmt.Sprintf("command[%d]", i), cmd, prefix, vars); err != nil {
				return err
			}
		}
		for i, arg := range step.Args {
			if err := validateTaskArraysIsolated(fmt.Sprintf("arg[%d]", i), arg, prefix, vars); err != nil {
				return err
			}
		}
		for _, env := range step.Env {
			if err := validateTaskNoArrayReferenced(fmt.Sprintf("env[%s]", env.Name), env.Value, prefix, vars); err != nil {
				return err
			}
		}
		for i, v := range step.VolumeMounts {
			if err := validateTaskNoArrayReferenced(fmt.Sprintf("volumeMount[%d].Name", i), v.Name, prefix, vars); err != nil {
				return err
			}
			if err := validateTaskNoArrayReferenced(fmt.Sprintf("volumeMount[%d].MountPath", i), v.MountPath, prefix, vars); err != nil {
				return err
			}
			if err := validateTaskNoArrayReferenced(fmt.Sprintf("volumeMount[%d].SubPath", i), v.SubPath, prefix, vars); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateVariables(steps []Step, prefix string, vars sets.String) *apis.FieldError {
	for _, step := range steps {
		if err := validateTaskVariable("name", step.Name, prefix, vars); err != nil {
			return err
		}
		if err := validateTaskVariable("image", step.Image, prefix, vars); err != nil {
			return err
		}
		if err := validateTaskVariable("workingDir", step.WorkingDir, prefix, vars); err != nil {
			return err
		}
		if err := validateTaskVariable("script", step.Script, prefix, vars); err != nil {
			return err
		}
		for i, cmd := range step.Command {
			if err := validateTaskVariable(fmt.Sprintf("command[%d]", i), cmd, prefix, vars); err != nil {
				return err
			}
		}
		for i, arg := range step.Args {
			if err := validateTaskVariable(fmt.Sprintf("arg[%d]", i), arg, prefix, vars); err != nil {
				return err
			}
		}
		for _, env := range step.Env {
			if err := validateTaskVariable(fmt.Sprintf("env[%s]", env.Name), env.Value, prefix, vars); err != nil {
				return err
			}
		}
		for i, v := range step.VolumeMounts {
			if err := validateTaskVariable(fmt.Sprintf("volumeMount[%d].Name", i), v.Name, prefix, vars); err != nil {
				return err
			}
			if err := validateTaskVariable(fmt.Sprintf("volumeMount[%d].MountPath", i), v.MountPath, prefix, vars); err != nil {
				return err
			}
			if err := validateTaskVariable(fmt.Sprintf("volumeMount[%d].SubPath", i), v.SubPath, prefix, vars); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateTaskVariable(name, value, prefix string, vars sets.String) *apis.FieldError {
	return substitution.ValidateVariable(name, value, prefix, "step", "taskspec.steps", vars)
}

func validateTaskNoArrayReferenced(name, value, prefix string, arrayNames sets.String) *apis.FieldError {
	return substitution.ValidateVariableProhibited(name, value, prefix, "step", "taskspec.steps", arrayNames)
}

func validateTaskArraysIsolated(name, value, prefix string, arrayNames sets.String) *apis.FieldError {
	return substitution.ValidateVariableIsolated(name, value, prefix, "step", "taskspec.steps", arrayNames)
}

func validateExpansion(ts *TaskSpec) *apis.FieldError {

	context, apiErr := createTaskContext(ts)
	if apiErr != nil {
		return apiErr
	}

	input, err := toJSONInterfaceType(ts)
	if err != nil {
		return apis.ErrGeneric(err.Error(), "spec")
	}

	_, err = jsonpath.Expand(input, context)
	if err != nil {
		return apis.ErrGeneric(err.Error(), "spec")
	}
	return nil
}

func toJSONInterfaceType(v interface{}) (interface{}, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var iJSON interface{}
	err = json.Unmarshal(b, &iJSON)
	if err != nil {
		return nil, err
	}
	return iJSON, nil

}

func createTaskContext(ts *TaskSpec) (interface{}, *apis.FieldError) {
	context := map[string]interface{}{}

	params, err := createParamsContext(ts.Params)
	if err != nil {
		return nil, apis.ErrGeneric(err.Error(), "params")
	}
	context["params"] = params

	workspaces, err := createWorkspacesContext(ts.Workspaces, params)
	if err != nil {
		return nil, apis.ErrGeneric(err.Error(), "workspaces")
	}
	context["workspaces"] = workspaces

	resources := map[string]interface{}{}

	if ts.Resources != nil {
		inputResources, err := createResourcesContext(ts.Resources.Inputs, false, params)
		if err != nil {
			return nil, apis.ErrGeneric(err.Error(), "resources.inputs")
		}
		resources["inputs"] = inputResources

		outputResources, err := createResourcesContext(ts.Resources.Outputs, true, params)
		if err != nil {
			return nil, apis.ErrGeneric(err.Error(), "resources.outputs")
		}
		resources["outputs"] = outputResources
	} else {
		resources["inputs"] = map[string]interface{}{}
		resources["outputs"] = map[string]interface{}{}
	}
	context["resources"] = resources

	results, err := createResultsContext(ts.Results, params)
	if err != nil {
		return nil, apis.ErrGeneric(err.Error(), "results")
	}
	context["results"] = results

	//backwards compatability -- inputs and outputs top-levels
	context["inputs"] = map[string]interface{}{
		"params":    params,
		"resources": resources["inputs"],
	}
	context["outputs"] = map[string]interface{}{
		"resources": resources["outputs"],
	}

	return context, nil
}

func createParamsContext(tsParams []ParamSpec) (map[string]interface{}, error) {
	params := map[string]interface{}{}
	if tsParams == nil {
		return params, nil
	}
	iParams, err := toJSONInterfaceType(tsParams)
	if err != nil {
		return nil, err
	}
	iParams, err = jsonpath.Expand(iParams, nil)
	if err != nil {
		return nil, err
	}

	for _, p := range iParams.([]interface{}) {
		param := p.(map[string]interface{})
		var name = param["name"].(string)
		var value interface{}
		if val, ok := param["default"]; ok {
			value = val
		} else {
			switch param["type"] {
			case "array":
				value = []interface{}{}
			default:
				value = ""
			}
		}
		params[name] = value
	}
	return params, nil
}

func createWorkspacesContext(tsWorkspaces []WorkspaceDeclaration, params interface{}) (map[string]interface{}, error) {
	workspaces := map[string]interface{}{}
	if tsWorkspaces == nil {
		return workspaces, nil
	}

	iWorkspaces, err := toJSONInterfaceType(tsWorkspaces)
	if err != nil {
		return nil, err
	}
	iWorkspaces, err = jsonpath.Expand(iWorkspaces, params)
	if err != nil {
		return nil, err
	}

	for _, w := range iWorkspaces.([]interface{}) {
		workspace := w.(map[string]interface{})
		var name = workspace["name"].(string)
		path := "/workspace/" + name
		if val, ok := workspace["mountPath"]; ok {
			path = val.(string)
		}
		workspaces[name] = map[string]interface{}{
			"path": path,
		}
	}
	return workspaces, nil
}

func computeResourcePath(resource map[string]interface{}, isOutput bool) string {
	if t, ok := resource["targetPath"]; ok && t != "" {
		targetPath := t.(string)
		if filepath.IsAbs(targetPath) {
			return targetPath
		}
		return filepath.Join("/workspace", targetPath)
	}
	var name = resource["name"].(string)
	if isOutput {
		return filepath.Join("/workspace/output", name)
	}
	return filepath.Join("/workspace", name)

}

func computeResourceParams(resource map[string]interface{}, isOutput bool) map[string]interface{} {
	resourceParams := map[string]interface{}{
		"path": computeResourcePath(resource, isOutput),
		"name": resource["name"].(string),
	}

	if t, ok := resource["type"]; ok && t != "" {
		resourceParams["type"] = t.(string)
		// var pri v1alpha1.PipelineResourceInterface
		switch t.(string) {
		case "git":
			//pri = &v1alpha1.GitResource{}
			resourceParams["url"] = ""
			resourceParams["revision"] = ""
			resourceParams["depth"] = ""
			resourceParams["sslVerify"] = ""
		case "image":
			//pri = &v1alpha1.ImageResource{}
			resourceParams["url"] = ""
			resourceParams["digest"] = ""
		case "cluster":
			//pri = &v1alpha1.ClusterResource{}
			resourceParams["url"] = ""
			resourceParams["revision"] = ""
			resourceParams["username"] = ""
			resourceParams["password"] = ""
			resourceParams["namespace"] = ""
			resourceParams["token"] = ""
			resourceParams["insecure"] = ""
			resourceParams["cadata"] = ""
		case "storage":
			//pri = &v1alpha1.GCSResource{}
			resourceParams["location"] = ""
		case "pullrequest":
			//pri = &v1alpha1.PullRequestResource{}
			resourceParams["url"] = ""
			resourceParams["provider"] = ""
			resourceParams["insecure-skip-tls-verify"] = ""
		case "cloudevent":
			//pri = &v1alpha1.CloudEventResource{}
			resourceParams["target-uri"] = ""
		}
		// if pri != nil {
		// 	for k, v := range pri.Replacements() {
		// 		resourceParams[k] = v
		// 	}
		// }
	}
	return resourceParams
}

func createResourcesContext(tsResources []TaskResource, isOutput bool, params interface{}) (map[string]interface{}, error) {
	resources := map[string]interface{}{}
	if tsResources == nil {
		return resources, nil
	}

	iResources, err := toJSONInterfaceType(tsResources)
	if err != nil {
		return nil, err
	}
	iResources, err = jsonpath.Expand(iResources, params)
	if err != nil {
		return nil, err
	}

	for _, r := range iResources.([]interface{}) {
		resource := r.(map[string]interface{})
		var name = resource["name"].(string)
		resources[name] = computeResourceParams(resource, isOutput)
	}

	return resources, nil
}

func createResultsContext(tsResults []TaskResult, params interface{}) (map[string]interface{}, error) {
	results := map[string]interface{}{}
	if tsResults == nil {
		return results, nil
	}

	iResults, err := toJSONInterfaceType(tsResults)
	if err != nil {
		return nil, err
	}
	iResults, err = jsonpath.Expand(iResults, params)
	if err != nil {
		return nil, err
	}

	for _, r := range iResults.(map[string]interface{}) {
		result := r.(map[string]interface{})
		var name = result["name"].(string)
		path := "/tekton/results/" + name
		if val, ok := result["path"]; ok {
			path = val.(string)
		}
		results[name] = map[string]interface{}{
			"path": path,
		}
	}
	return results, nil
}
