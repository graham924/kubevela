/*
Copyright 2021 The KubeVela Authors.

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

package appfile

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/format"
	json2cue "cuelang.org/go/encoding/json"
	"github.com/crossplane/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplane/crossplane-runtime/pkg/fieldpath"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1alpha2"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/apis/types"
	"github.com/oam-dev/kubevela/pkg/appfile/config"
	"github.com/oam-dev/kubevela/pkg/appfile/helm"
	"github.com/oam-dev/kubevela/pkg/controller/utils"
	"github.com/oam-dev/kubevela/pkg/dsl/definition"
	"github.com/oam-dev/kubevela/pkg/dsl/process"
	"github.com/oam-dev/kubevela/pkg/oam"
	"github.com/oam-dev/kubevela/pkg/oam/discoverymapper"
	"github.com/oam-dev/kubevela/pkg/oam/util"
)

const (
	// AppfileBuiltinConfig defines the built-in config variable
	AppfileBuiltinConfig = "config"
)

// constant error information
const (
	errInvalidValueType = "require %q type parameter value"
)

// Workload is component
type Workload struct {
	Name               string
	Type               string
	CapabilityCategory types.CapabilityCategory
	Params             map[string]interface{}
	Traits             []*Trait
	Scopes             []Scope

	Template           string
	HealthCheckPolicy  string
	CustomStatusFormat string

	Helm                *common.Helm
	DefinitionReference common.WorkloadGVK
	// TODO: remove all the duplicate fields above as workload now contains the whole template
	FullTemplate *util.Template

	engine definition.AbstractEngine
	// OutputSecretName is the secret name which this workload will generate after it successfully generate a cloud resource
	OutputSecretName string
	// RequiredSecrets stores secret names which the workload needs from cloud resource component and its context
	RequiredSecrets []process.RequiredSecrets
}

// GetUserConfigName get user config from AppFile, it will contain config file in it.
func (wl *Workload) GetUserConfigName() string {
	if wl.Params == nil {
		return ""
	}
	t, ok := wl.Params[AppfileBuiltinConfig]
	if !ok {
		return ""
	}
	ts, ok := t.(string)
	if !ok {
		return ""
	}
	return ts
}

// EvalContext eval workload template and set result to context
func (wl *Workload) EvalContext(ctx process.Context) error {
	return wl.engine.Complete(ctx, wl.Template, wl.Params)
}

// EvalStatus eval workload status
func (wl *Workload) EvalStatus(ctx process.Context, cli client.Client, ns string) (string, error) {
	return wl.engine.Status(ctx, cli, ns, wl.CustomStatusFormat)
}

// EvalHealth eval workload health check
func (wl *Workload) EvalHealth(ctx process.Context, client client.Client, namespace string) (bool, error) {
	return wl.engine.HealthCheck(ctx, client, namespace, wl.HealthCheckPolicy)
}

// Scope defines the scope of workload
type Scope struct {
	Name string
	GVK  schema.GroupVersionKind
}

// Trait is ComponentTrait
type Trait struct {
	// The Name is name of TraitDefinition, actually it's a type of the trait instance
	Name               string
	CapabilityCategory types.CapabilityCategory
	Params             map[string]interface{}

	Template           string
	HealthCheckPolicy  string
	CustomStatusFormat string

	FullTemplate *util.Template
	engine       definition.AbstractEngine
}

// EvalContext eval trait template and set result to context
func (trait *Trait) EvalContext(ctx process.Context) error {
	return trait.engine.Complete(ctx, trait.Template, trait.Params)
}

// EvalStatus eval trait status
func (trait *Trait) EvalStatus(ctx process.Context, cli client.Client, ns string) (string, error) {
	return trait.engine.Status(ctx, cli, ns, trait.CustomStatusFormat)
}

// EvalHealth eval trait health check
func (trait *Trait) EvalHealth(ctx process.Context, client client.Client, namespace string) (bool, error) {
	return trait.engine.HealthCheck(ctx, client, namespace, trait.HealthCheckPolicy)
}

// Appfile describes application
type Appfile struct {
	// Name application的名称
	Name string
	// RevisionName 当前Application资源的修订资源名称
	RevisionName string
	// Workloads 记录application的所有workload
	Workloads []*Workload
}

// TemplateValidate validate Template format
func (af *Appfile) TemplateValidate() error {
	return nil
}

// Parser is an application parser
type Parser struct {
	client client.Client
	dm     discoverymapper.DiscoveryMapper
	pd     *definition.PackageDiscover
}

// NewApplicationParser create appfile parser
func NewApplicationParser(cli client.Client, dm discoverymapper.DiscoveryMapper, pd *definition.PackageDiscover) *Parser {
	return &Parser{
		client: cli,
		dm:     dm,
		pd:     pd,
	}
}

// GenerateAppFile converts an application to an Appfile
// 解析application，并生成一个Appfile
func (p *Parser) GenerateAppFile(ctx context.Context, name string, app *v1beta1.Application) (*Appfile, error) {
	// 新建一个Appfile
	appfile := new(Appfile)
	// 设置 appfile.Name
	appfile.Name = name
	// 遍历 application.Spec.Components，解析每一个组件，得到一个Workload数组
	var wds []*Workload
	for _, comp := range app.Spec.Components {
		wd, err := p.parseWorkload(ctx, comp)
		if err != nil {
			return nil, err
		}
		wds = append(wds, wd)
	}
	// 将解析得到的workload数组，赋值给 appfile.Workloads
	appfile.Workloads = wds
	// 返回解析得到的Appfile
	return appfile, nil
}

// parseWorkload 将 ApplicationComponent 解析成一个Workload
func (p *Parser) parseWorkload(ctx context.Context, comp v1beta1.ApplicationComponent) (*Workload, error) {

	templ, err := util.LoadTemplate(ctx, p.dm, p.client, comp.Type, types.TypeComponentDefinition)
	if err != nil && !kerrors.IsNotFound(err) {
		return nil, errors.WithMessagef(err, "fetch type of %s", comp.Name)
	}
	settings, err := util.RawExtension2Map(&comp.Properties)
	if err != nil {
		return nil, errors.WithMessagef(err, "fail to parse settings for %s", comp.Name)
	}
	workload := &Workload{
		Traits:              []*Trait{},
		Name:                comp.Name,
		Type:                comp.Type,
		CapabilityCategory:  templ.CapabilityCategory,
		Template:            templ.TemplateStr,
		HealthCheckPolicy:   templ.Health,
		CustomStatusFormat:  templ.CustomStatus,
		DefinitionReference: templ.Reference,
		Helm:                templ.Helm,
		FullTemplate:        templ,
		Params:              settings,
		engine:              definition.NewWorkloadAbstractEngine(comp.Name, p.pd),
	}
	for _, traitValue := range comp.Traits {
		properties, err := util.RawExtension2Map(&traitValue.Properties)
		if err != nil {
			return nil, errors.Errorf("fail to parse properties of %s for %s", traitValue.Type, comp.Name)
		}
		trait, err := p.parseTrait(ctx, traitValue.Type, properties)
		if err != nil {
			return nil, errors.WithMessagef(err, "component(%s) parse trait(%s)", comp.Name, traitValue.Type)
		}

		workload.Traits = append(workload.Traits, trait)
	}
	for scopeType, instanceName := range comp.Scopes {
		gvk, err := util.GetScopeGVK(ctx, p.client, p.dm, scopeType)
		if err != nil {
			return nil, err
		}
		workload.Scopes = append(workload.Scopes, Scope{
			Name: instanceName,
			GVK:  gvk,
		})
	}
	return workload, nil
}

func (p *Parser) parseTrait(ctx context.Context, name string, properties map[string]interface{}) (*Trait, error) {
	templ, err := util.LoadTemplate(ctx, p.dm, p.client, name, types.TypeTrait)
	if kerrors.IsNotFound(err) {
		return nil, errors.Errorf("trait definition of %s not found", name)
	}
	if err != nil {
		return nil, err
	}
	return &Trait{
		Name:               name,
		CapabilityCategory: templ.CapabilityCategory,
		Params:             properties,
		Template:           templ.TemplateStr,
		HealthCheckPolicy:  templ.Health,
		CustomStatusFormat: templ.CustomStatus,
		FullTemplate:       templ,
		engine:             definition.NewTraitAbstractEngine(name, p.pd),
	}, nil
}

// GenerateApplicationConfiguration converts an appFile to applicationConfig & Components
func (p *Parser) GenerateApplicationConfiguration(app *Appfile, ns string) (*v1alpha2.ApplicationConfiguration,
	[]*v1alpha2.Component, error) {
	appconfig := &v1alpha2.ApplicationConfiguration{}
	appconfig.SetGroupVersionKind(v1alpha2.ApplicationConfigurationGroupVersionKind)
	appconfig.Name = app.Name
	appconfig.Namespace = ns

	if appconfig.Labels == nil {
		appconfig.Labels = map[string]string{}
	}
	appconfig.Labels[oam.LabelAppName] = app.Name

	var components []*v1alpha2.Component
	ctx := context.Background()

	for _, wl := range app.Workloads {
		var (
			comp   *v1alpha2.Component
			acComp *v1alpha2.ApplicationConfigurationComponent
			err    error
		)

		if wl.IsCloudResourceConsumer() {
			requiredSecrets, err := parseWorkloadInsertSecretTo(ctx, p.client, ns, wl)
			if err != nil {
				return nil, nil, err
			}
			wl.RequiredSecrets = requiredSecrets
		}

		switch wl.CapabilityCategory {
		case types.HelmCategory:
			comp, acComp, err = generateComponentFromHelmModule(p.client, wl, app.Name, app.RevisionName, ns)
			if err != nil {
				return nil, nil, err
			}
		case types.KubeCategory:
			comp, acComp, err = generateComponentFromKubeModule(p.client, wl, app.Name, app.RevisionName, ns)
			if err != nil {
				return nil, nil, err
			}
		default:
			comp, acComp, err = generateComponentFromCUEModule(p.client, wl, app.Name, app.RevisionName, ns)
			if err != nil {
				return nil, nil, err
			}
		}
		components = append(components, comp)
		appconfig.Spec.Components = append(appconfig.Spec.Components, *acComp)
	}
	return appconfig, components, nil
}

func generateComponentFromCUEModule(c client.Client, wl *Workload, appName, revision, ns string) (*v1alpha2.Component, *v1alpha2.ApplicationConfigurationComponent, error) {
	var (
		outputSecretName string
		err              error
	)
	if wl.IsCloudResourceProducer() {
		outputSecretName, err = GetOutputSecretNames(wl)
		if err != nil {
			return nil, nil, err
		}
		wl.OutputSecretName = outputSecretName
	}
	pCtx, err := PrepareProcessContext(c, wl, appName, revision, ns)
	if err != nil {
		return nil, nil, err
	}
	for _, tr := range wl.Traits {
		if err := tr.EvalContext(pCtx); err != nil {
			return nil, nil, errors.Wrapf(err, "evaluate template trait=%s app=%s", tr.Name, wl.Name)
		}
	}
	var comp *v1alpha2.Component
	var acComp *v1alpha2.ApplicationConfigurationComponent
	comp, acComp, err = evalWorkloadWithContext(pCtx, wl, appName, wl.Name)
	if err != nil {
		return nil, nil, err
	}
	comp.Name = wl.Name
	acComp.ComponentName = comp.Name

	for _, sc := range wl.Scopes {
		acComp.Scopes = append(acComp.Scopes, v1alpha2.ComponentScope{ScopeReference: v1alpha1.TypedReference{
			APIVersion: sc.GVK.GroupVersion().String(),
			Kind:       sc.GVK.Kind,
			Name:       sc.Name,
		}})
	}
	if len(comp.Namespace) == 0 {
		comp.Namespace = ns
	}
	if comp.Labels == nil {
		comp.Labels = map[string]string{}
	}
	comp.Labels[oam.LabelAppName] = appName
	comp.SetGroupVersionKind(v1alpha2.ComponentGroupVersionKind)

	return comp, acComp, nil
}

func generateComponentFromKubeModule(c client.Client, wl *Workload, appName, revision, ns string) (*v1alpha2.Component, *v1alpha2.ApplicationConfigurationComponent, error) {
	kubeObj := &unstructured.Unstructured{}
	err := json.Unmarshal(wl.FullTemplate.Kube.Template.Raw, kubeObj)
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot decode Kube template into K8s object")
	}

	paramValues, err := resolveKubeParameters(wl.FullTemplate.Kube.Parameters, wl.Params)
	if err != nil {
		return nil, nil, errors.WithMessage(err, "cannot resolve parameter settings")
	}
	if err := setParameterValuesToKubeObj(kubeObj, paramValues); err != nil {
		return nil, nil, errors.WithMessage(err, "cannot set parameters value")
	}

	// convert structured kube obj into CUE (go ==marshal==> json ==decoder==> cue)
	objRaw, err := kubeObj.MarshalJSON()
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot marshal kube object")
	}
	ins, err := json2cue.Decode(&cue.Runtime{}, "", objRaw)
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot decode object into CUE")
	}
	cueRaw, err := format.Node(ins.Value().Syntax())
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot format CUE")
	}

	// NOTE a hack way to enable using CUE capabilities on KUBE schematic workload
	wl.Template = fmt.Sprintf(`
output: { 
	%s 
}`, string(cueRaw))

	// re-use the way CUE module generates comp & acComp
	comp, acComp, err := generateComponentFromCUEModule(c, wl, appName, revision, ns)
	if err != nil {
		return nil, nil, err
	}
	return comp, acComp, nil
}

// a helper map whose key is parameter name
type paramValueSettings map[string]paramValueSetting
type paramValueSetting struct {
	Value      interface{}
	ValueType  common.ParameterValueType
	FieldPaths []string
}

func resolveKubeParameters(params []common.KubeParameter, settings map[string]interface{}) (paramValueSettings, error) {
	supported := map[string]*common.KubeParameter{}
	for _, p := range params {
		supported[p.Name] = p.DeepCopy()
	}

	values := make(paramValueSettings)
	for name, v := range settings {
		// check unsupported parameter setting
		if supported[name] == nil {
			return nil, errors.Errorf("unsupported parameter %q", name)
		}
		// construct helper map
		values[name] = paramValueSetting{
			Value:      v,
			ValueType:  supported[name].ValueType,
			FieldPaths: supported[name].FieldPaths,
		}
	}

	// check required parameter
	for _, p := range params {
		if p.Required != nil && *p.Required {
			if _, ok := values[p.Name]; !ok {
				return nil, errors.Errorf("require parameter %q", p.Name)
			}
		}
	}
	return values, nil
}

func setParameterValuesToKubeObj(obj *unstructured.Unstructured, values paramValueSettings) error {
	paved := fieldpath.Pave(obj.Object)
	for paramName, v := range values {
		for _, f := range v.FieldPaths {
			switch v.ValueType {
			case common.StringType:
				vString, ok := v.Value.(string)
				if !ok {
					return errors.Errorf(errInvalidValueType, v.ValueType)
				}
				if err := paved.SetString(f, vString); err != nil {
					return errors.Wrapf(err, "cannot set parameter %q to field %q", paramName, f)
				}
			case common.NumberType:
				switch v.Value.(type) {
				case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
					if err := paved.SetValue(f, v.Value); err != nil {
						return errors.Wrapf(err, "cannot set parameter %q to field %q", paramName, f)
					}
				default:
					return errors.Errorf(errInvalidValueType, v.ValueType)
				}
			case common.BooleanType:
				vBoolean, ok := v.Value.(bool)
				if !ok {
					return errors.Errorf(errInvalidValueType, v.ValueType)
				}
				if err := paved.SetValue(f, vBoolean); err != nil {
					return errors.Wrapf(err, "cannot set parameter %q to field %q", paramName, f)
				}
			}
		}
	}
	return nil
}

func generateComponentFromHelmModule(c client.Client, wl *Workload, appName, revision, ns string) (*v1alpha2.Component, *v1alpha2.ApplicationConfigurationComponent, error) {
	gv, err := schema.ParseGroupVersion(wl.DefinitionReference.APIVersion)
	if err != nil {
		return nil, nil, err
	}
	targetWokrloadGVK := gv.WithKind(wl.DefinitionReference.Kind)

	// NOTE this is a hack way to enable using CUE module capabilities on Helm module workload
	// construct an empty base workload according to its GVK
	wl.Template = fmt.Sprintf(`
output: {
	apiVersion: "%s"
	kind: "%s"
}`, targetWokrloadGVK.GroupVersion().String(), targetWokrloadGVK.Kind)

	// re-use the way CUE module generates comp & acComp
	comp, acComp, err := generateComponentFromCUEModule(c, wl, appName, revision, ns)
	if err != nil {
		return nil, nil, err
	}

	release, repo, err := helm.RenderHelmReleaseAndHelmRepo(wl.Helm, wl.Name, appName, ns, wl.Params)
	if err != nil {
		return nil, nil, err
	}
	rlsBytes, err := json.Marshal(release.Object)
	if err != nil {
		return nil, nil, err
	}
	repoBytes, err := json.Marshal(repo.Object)
	if err != nil {
		return nil, nil, err
	}
	comp.Spec.Helm = &common.Helm{
		Release:    runtime.RawExtension{Raw: rlsBytes},
		Repository: runtime.RawExtension{Raw: repoBytes},
	}
	return comp, acComp, nil
}

// evalWorkloadWithContext evaluate the workload's template to generate component and ACComponent
func evalWorkloadWithContext(pCtx process.Context, wl *Workload, appName, compName string) (*v1alpha2.Component, *v1alpha2.ApplicationConfigurationComponent, error) {
	base, assists := pCtx.Output()
	componentWorkload, err := base.Unstructured()
	if err != nil {
		return nil, nil, errors.Wrapf(err, "evaluate base template component=%s app=%s", compName, appName)
	}

	var commonLabels = definition.GetCommonLabels(pCtx.BaseContextLabels())
	util.AddLabels(componentWorkload, util.MergeMapOverrideWithDst(commonLabels, map[string]string{oam.WorkloadTypeLabel: wl.Type}))

	component := &v1alpha2.Component{}
	// we need to marshal the workload to byte array before sending them to the k8s
	component.Spec.Workload = util.Object2RawExtension(componentWorkload)

	acComponent := &v1alpha2.ApplicationConfigurationComponent{}
	for _, assist := range assists {
		tr, err := assist.Ins.Unstructured()
		if err != nil {
			return nil, nil, errors.Wrapf(err, "evaluate trait=%s template for component=%s app=%s", assist.Name, compName, appName)
		}
		labels := util.MergeMapOverrideWithDst(commonLabels, map[string]string{oam.TraitTypeLabel: assist.Type})
		if assist.Name != "" {
			labels[oam.TraitResource] = assist.Name
		}
		util.AddLabels(tr, labels)
		acComponent.Traits = append(acComponent.Traits, v1alpha2.ComponentTrait{
			// we need to marshal the trait to byte array before sending them to the k8s
			Trait: util.Object2RawExtension(tr),
		})
	}
	return component, acComponent, nil
}

// PrepareProcessContext prepares a DSL process Context
func PrepareProcessContext(k8sClient client.Client, wl *Workload, applicationName, revision, namespace string) (process.Context, error) {
	pCtx := process.NewContext(namespace, wl.Name, applicationName, revision)
	pCtx.InsertSecrets(wl.OutputSecretName, wl.RequiredSecrets)
	userConfig := wl.GetUserConfigName()
	if userConfig != "" {
		cg := config.Configmap{Client: k8sClient}
		// TODO(wonderflow): envName should not be namespace when we have serverside env
		var envName = namespace
		data, err := cg.GetConfigData(config.GenConfigMapName(applicationName, wl.Name, userConfig), envName)
		if err != nil {
			return nil, errors.Wrapf(err, "get config=%s for app=%s in namespace=%s", userConfig, applicationName, namespace)
		}
		pCtx.SetConfigs(data)
	}
	if err := wl.EvalContext(pCtx); err != nil {
		return nil, errors.Wrapf(err, "evaluate base template app=%s in namespace=%s", applicationName, namespace)
	}
	return pCtx, nil
}

// GetOutputSecretNames set all secret names, which are generated by cloud resource, to context
func GetOutputSecretNames(workloads *Workload) (string, error) {
	secretName, err := getComponentSetting(process.OutputSecretName, workloads.Params)
	if err != nil {
		return "", err
	}

	return fmt.Sprint(secretName), nil
}

func parseWorkloadInsertSecretTo(ctx context.Context, c client.Client, namespace string, wl *Workload) ([]process.RequiredSecrets, error) {
	var requiredSecret []process.RequiredSecrets
	api, err := utils.GenerateOpenAPISchemaFromDefinition(wl.Name, wl.Template)
	if err != nil {
		if !errors.Is(err, errors.Errorf(utils.ErrNoSectionParameterInCue, wl.Name)) {
			return nil, nil
		}
		return nil, err
	}

	schema, err := utils.ConvertOpenAPISchema2SwaggerObject(api)
	if err != nil {
		return nil, err
	}
	for k, v := range schema.Properties {
		description := v.Value.Description
		if strings.Contains(description, utils.InsertSecretToTag) {
			contextName := strings.Split(description, utils.InsertSecretToTag)[1]
			contextName = strings.TrimSpace(contextName)
			secretNameInterface, err := getComponentSetting(k, wl.Params)
			if err != nil {
				return nil, err
			}
			secretName, ok := secretNameInterface.(string)
			if !ok {
				return nil, fmt.Errorf("failed to convert secret name %v to string", secretNameInterface)
			}
			secretData, err := extractSecret(ctx, c, namespace, secretName)
			if err != nil {
				return nil, err
			}

			requiredSecret = append(requiredSecret, process.RequiredSecrets{
				Name:        secretName,
				ContextName: contextName,
				Namespace:   namespace,
				Data:        secretData,
			})
		}
	}
	return requiredSecret, nil
}

func extractSecret(ctx context.Context, c client.Client, namespace, name string) (map[string]interface{}, error) {
	secretData := make(map[string]interface{})
	var secret v1.Secret
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &secret); err != nil {
		return nil, fmt.Errorf("failed to get secret %s from namespace %s which is required by the component: %w",
			name, namespace, err)
	}
	for k, v := range secret.Data {
		secretData[k] = string(v)
	}
	if len(secretData) == 0 {
		return nil, fmt.Errorf("data in secret %s from namespace %s isn't available", name, namespace)
	}
	return secretData, nil
}

func getComponentSetting(settingParamName string, params map[string]interface{}) (interface{}, error) {
	if secretName, ok := params[settingParamName]; ok {
		return secretName, nil
	}
	return nil, fmt.Errorf("failed to get the value of component setting %s", settingParamName)
}

// IsCloudResourceProducer checks whether a workload is cloud resource producer role
func (wl *Workload) IsCloudResourceProducer() bool {
	var existed bool
	_, existed = wl.Params[process.OutputSecretName]
	return existed
}

// IsCloudResourceConsumer checks whether a workload is cloud resource consumer role
func (wl *Workload) IsCloudResourceConsumer() bool {
	requiredSecretTag := strings.TrimRight(utils.InsertSecretToTag, "=")
	matched, err := regexp.Match(regexp.QuoteMeta(requiredSecretTag), []byte(wl.Template))
	if err != nil || !matched {
		return false
	}
	return true
}
