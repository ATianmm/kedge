/*
Copyright 2017 The Kedge Authors All rights reserved.

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

package kubernetes

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/kedgeproject/kedge/pkg/spec"

	log "github.com/Sirupsen/logrus"
	"github.com/davecgh/go-spew/spew"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/pkg/api"
	api_v1 "k8s.io/client-go/pkg/api/v1"
	ext_v1beta1 "k8s.io/client-go/pkg/apis/extensions/v1beta1"

	// install api (register and add types to api.Schema)
	_ "k8s.io/client-go/pkg/api/install"
	_ "k8s.io/client-go/pkg/apis/extensions/install"
)

func getLabels(app *spec.App) map[string]string {
	labels := map[string]string{"app": app.Name}
	return labels
}

func createIngresses(app *spec.App) ([]runtime.Object, error) {
	var ings []runtime.Object

	for _, i := range app.Ingresses {
		ing := &ext_v1beta1.Ingress{
			ObjectMeta: metav1.ObjectMeta{
				Name:   i.Name,
				Labels: app.Labels,
			},
			Spec: i.IngressSpec,
		}
		ings = append(ings, ing)
	}
	return ings, nil
}

func createServices(app *spec.App) ([]runtime.Object, error) {
	var svcs []runtime.Object
	for _, s := range app.Services {
		svc := &api_v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:   s.Name,
				Labels: app.Labels,
			},
			Spec: s.ServiceSpec,
		}
		for _, servicePortMod := range s.Ports {
			svc.Spec.Ports = append(svc.Spec.Ports, servicePortMod.ServicePort)
		}
		if len(svc.Spec.Selector) == 0 {
			svc.Spec.Selector = app.Labels
		}
		svcs = append(svcs, svc)

		// Generate ingress if "endpoint" is mentioned in app.Services.Ports[].Endpoint
		for _, port := range s.Ports {
			if port.Endpoint != "" {
				var host string
				var path string
				endpoint := strings.SplitN(port.Endpoint, "/", 2)
				switch len(endpoint) {
				case 1:
					host = endpoint[0]
					path = "/"
				case 2:
					host = endpoint[0]
					path = "/" + endpoint[1]
				default:
					return nil, fmt.Errorf("Invalid syntax for endpoint: %v", port.Endpoint)
				}

				ingressName := s.Name + "-" + strconv.FormatInt(int64(port.Port), 10)
				endpointIngress := &ext_v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:   ingressName,
						Labels: app.Labels,
					},
					Spec: ext_v1beta1.IngressSpec{
						Rules: []ext_v1beta1.IngressRule{
							{
								Host: host,
								IngressRuleValue: ext_v1beta1.IngressRuleValue{
									HTTP: &ext_v1beta1.HTTPIngressRuleValue{
										Paths: []ext_v1beta1.HTTPIngressPath{
											{
												Path: path,
												Backend: ext_v1beta1.IngressBackend{
													ServiceName: s.Name,
													ServicePort: intstr.IntOrString{
														IntVal: port.Port,
													},
												},
											},
										},
									},
								},
							},
						},
					},
				}
				svcs = append(svcs, endpointIngress)
			}
		}
	}
	return svcs, nil
}

// Creates a Deployment Kubernetes resource. The returned Deployment resource
// will be nil if it could not be generated due to insufficient input data.
func createDeployment(app *spec.App) (*ext_v1beta1.Deployment, error) {

	// We need to error out if both, app.PodSpec and app.DeploymentSpec are empty
	if reflect.DeepEqual(app.PodSpec, api_v1.PodSpec{}) && reflect.DeepEqual(app.DeploymentSpec, ext_v1beta1.DeploymentSpec{}) {
		log.Debug("Both, app.PodSpec and app.DeploymentSpec are empty, not enough data to create a deployment.")
		return nil, nil
	}

	// We are merging whole DeploymentSpec with PodSpec.
	// This means that someone could specify containers in template.spec and also in top level PodSpec.
	// This stupid check is supposed to make sure that only one of them set.
	// TODO: merge DeploymentSpec.Template.Spec and top level PodSpec
	if !(reflect.DeepEqual(app.DeploymentSpec.Template.Spec, api_v1.PodSpec{}) || reflect.DeepEqual(app.PodSpec, api_v1.PodSpec{})) {
		return nil, fmt.Errorf("Pod can't be specfied in two places. Use top level PodSpec or template.spec (DeploymentSpec.Template.Spec) not both")
	}

	deploymentSpec := app.DeploymentSpec

	// top level PodSpec is not empty, use it for deployment template
	// we already know that if app.PodSpec is not empty app.DeploymentSpec.Template.Spec is empty
	if !reflect.DeepEqual(app.PodSpec, api_v1.PodSpec{}) {
		deploymentSpec.Template.Spec = app.PodSpec
	}

	// TODO: check if this wasn't set by user, in that case we shouldn't ovewrite it
	deploymentSpec.Template.ObjectMeta.Name = app.Name

	// TODO: merge with already existing labels and avoid duplication
	deploymentSpec.Template.ObjectMeta.Labels = app.Labels

	deployment := ext_v1beta1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:   app.Name,
			Labels: app.Labels,
		},
		Spec: deploymentSpec,
	}

	return &deployment, nil
}

// search through all the persistent volumes defined in the root level
func isPVCDefined(app *spec.App, name string) bool {
	for _, v := range app.VolumeClaims {
		if v.Name == name {
			return true
		}
	}
	return false
}

// create PVC reading the root level persistent volume field
func createPVC(v spec.VolumeClaim, labels map[string]string) (*api_v1.PersistentVolumeClaim, error) {
	// check for conditions where user has given both conflicting fields
	// or not given either fields
	if v.Size != "" && v.Resources.Requests != nil {
		return nil, fmt.Errorf("persistent volume %q, cannot provide size and resources at the same time", v.Name)
	}
	if v.Size == "" && v.Resources.Requests == nil {
		return nil, fmt.Errorf("persistent volume %q, please provide size or resources, none given", v.Name)
	}

	// if user has given size then create a "api_v1.ResourceRequirements"
	// because this can be fed to pvc directly
	if v.Size != "" {
		size, err := resource.ParseQuantity(v.Size)
		if err != nil {
			return nil, errors.Wrap(err, "could not read volume size")
		}
		// update the volume's resource so that it can be fed
		v.Resources = api_v1.ResourceRequirements{
			Requests: api_v1.ResourceList{
				api_v1.ResourceStorage: size,
			},
		}
	}
	// setting the default accessmode if none given by user
	if len(v.AccessModes) == 0 {
		v.AccessModes = []api_v1.PersistentVolumeAccessMode{api_v1.ReadWriteOnce}
	}
	pvc := &api_v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:   v.Name,
			Labels: labels,
		},
		// since we updated the pvc spec before so this can be directly fed
		// without having to do any addition extra
		Spec: api_v1.PersistentVolumeClaimSpec(v.PersistentVolumeClaimSpec),
	}
	return pvc, nil
}

// This function will search in the pod level volumes
// and see if the volume with given name is defined
func isVolumeDefined(app *spec.App, name string) bool {
	for _, v := range app.Volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

// Since we are automatically creating pvc from
// root level persistent volume and entry in the container
// volume mount, we also need to update the pod's volume field
func populateVolumes(app *spec.App) error {
	for cn, c := range app.PodSpec.Containers {
		for vn, vm := range c.VolumeMounts {
			if isPVCDefined(app, vm.Name) && !isVolumeDefined(app, vm.Name) {
				app.Volumes = append(app.Volumes, api_v1.Volume{
					Name: vm.Name,
					VolumeSource: api_v1.VolumeSource{
						PersistentVolumeClaim: &api_v1.PersistentVolumeClaimVolumeSource{
							ClaimName: vm.Name,
						},
					},
				})
			} else if !isVolumeDefined(app, vm.Name) {
				// pvc is not defined so we need to check if the entry is made in the pod volumes
				// since a volumeMount entry without entry in pod level volumes might cause failure
				// while deployment since that would not be a complete configuration
				return fmt.Errorf("neither root level Persistent Volume"+
					" nor Volume in pod spec defined for %q, "+
					"in app.containers[%d].volumeMounts[%d]", vm.Name, cn, vn)
			}
		}
	}
	return nil
}

func populateContainerHealth(app *spec.App) error {
	for cn, c := range app.Containers {
		// check if health and liveness given together
		if c.Health != nil && (c.ReadinessProbe != nil || c.LivenessProbe != nil) {
			return fmt.Errorf("cannot define field health and livnessProbe"+
				" or readinessProbe together in app.containers[%d]", cn)
		}
		if c.Health != nil {
			c.LivenessProbe = c.Health
			c.ReadinessProbe = c.Health
		}
		app.PodSpec.Containers = append(app.PodSpec.Containers, c.Container)
	}
	return nil
}

func getConfigMapData(name string, configMaps []spec.ConfigMapMod) (map[string]string, error) {
	for _, cm := range configMaps {
		if cm.Name == name {
			return cm.Data, nil
		}
	}
	return nil, fmt.Errorf("configMap %v not defined", name)
}

func getSecretDataKeys(name string, secrets []spec.SecretMod) ([]string, error) {
	var dataKeys []string
	for _, secret := range secrets {
		if secret.Name == name {
			for dk := range secret.Data {
				dataKeys = append(dataKeys, dk)
			}
			for sdk := range secret.StringData {
				dataKeys = append(dataKeys, sdk)
			}
			return dataKeys, nil
		}
	}
	return nil, fmt.Errorf("secret %v not defined", name)
}

func populateEnvFrom(app *spec.App) error {
	// iterate on the containers so that we can extract envFrom
	// that we have custom defined
	for ci, c := range app.Containers {
		for _, e := range c.EnvFrom {
			var envs []api_v1.EnvVar

			// populating ConfigMaps
			if e.ConfigMapRef != nil {
				rootConfigMapData, err := getConfigMapData(e.ConfigMapRef.Name, app.ConfigMaps)
				if err != nil {
					return errors.Wrapf(err, "unable to get configMap: %v", e.ConfigMapRef.Name)
				}

				// start populating
				for configMapDataKey := range rootConfigMapData {
					// here we are directly referring to the containers
					// from app.PodSpec.Containers because that is where data
					// is parsed into so populating that is more valid thing to do
					envs = append(envs, api_v1.EnvVar{
						Name: configMapDataKey,
						ValueFrom: &api_v1.EnvVarSource{
							ConfigMapKeyRef: &api_v1.ConfigMapKeySelector{
								LocalObjectReference: api_v1.LocalObjectReference{
									Name: e.ConfigMapRef.Name,
								},
								Key: configMapDataKey,
							},
						},
					})
				}
			}

			// populating secrets
			if e.SecretRef != nil {
				rootSecretDataKeys, err := getSecretDataKeys(e.SecretRef.Name, app.Secrets)
				if err != nil {
					return errors.Wrap(err, "unable to get secret data")
				}

				for _, secretDataKey := range rootSecretDataKeys {
					envs = append(envs, api_v1.EnvVar{
						Name: secretDataKey,
						ValueFrom: &api_v1.EnvVarSource{
							SecretKeyRef: &api_v1.SecretKeySelector{
								LocalObjectReference: api_v1.LocalObjectReference{
									Name: e.SecretRef.Name,
								},
								Key: secretDataKey,
							},
						},
					})
				}
			}

			// we collect all the envs from configMap and secret before
			// envs provided inside the container
			envs = append(envs, app.PodSpec.Containers[ci].Env...)
			app.PodSpec.Containers[ci].Env = envs
		}

		// Since we are not supporting envFrom in our generated Kubernetes
		// artifacts right now, we need to set it as nil for every container.
		// This makes sure that Kubernetes artifacts do not container an
		// envFrom field.
		// This is safe to set since all of the data from envFrom has been
		// extracted till this point.
		if app.PodSpec.Containers[ci].EnvFrom != nil {
			app.PodSpec.Containers[ci].EnvFrom = nil
		}
	}
	return nil
}

func createSecrets(app *spec.App) ([]runtime.Object, error) {
	var secrets []runtime.Object

	for _, s := range app.Secrets {
		secret := &api_v1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:   s.Name,
				Labels: app.Labels,
			},
			Data:       s.Data,
			StringData: s.StringData,
			Type:       s.Type,
		}
		secrets = append(secrets, secret)
	}
	return secrets, nil
}

// CreateK8sObjects, if given object spec.App, this function reads
// them and returns kubernetes objects as list of runtime.Object
// If the app is using field 'extraResources' then it will
// also return file names mentioned there as list of string
func CreateK8sObjects(app *spec.App) ([]runtime.Object, []string, error) {
	var objects []runtime.Object

	if app.Labels == nil {
		app.Labels = getLabels(app)
	}

	svcs, err := createServices(app)
	if err != nil {
		return nil, nil, errors.Wrap(err, "Unable to create Kubernetes Service")
	}

	ings, err := createIngresses(app)
	if err != nil {
		return nil, nil, errors.Wrap(err, "Unable to create Kubernetes Ingresses")
	}

	secs, err := createSecrets(app)
	if err != nil {
		return nil, nil, errors.Wrap(err, "Unable to create Kubernetes Secrets")
	}

	// withdraw the health and populate actual pod spec
	if err := populateContainerHealth(app); err != nil {
		return nil, nil, errors.Wrapf(err, "app %q", app.Name)
	}
	log.Debugf("object after population: %#v\n", app)

	// withdraw the envFrom and populate actual containers
	if err := populateEnvFrom(app); err != nil {
		return nil, nil, errors.Wrapf(err, "app %q", app.Name)
	}

	// create pvc for each root level persistent volume
	var pvcs []runtime.Object
	for _, v := range app.VolumeClaims {
		pvc, err := createPVC(v, app.Labels)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "app %q", app.Name)
		}
		pvcs = append(pvcs, pvc)
	}
	if err := populateVolumes(app); err != nil {
		return nil, nil, errors.Wrapf(err, "app %q", app.Name)
	}

	var configMap []runtime.Object
	for _, cd := range app.ConfigMaps {
		cm := &api_v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:   cd.Name,
				Labels: app.Labels,
			},
			Data: cd.Data,
		}

		configMap = append(configMap, cm)
	}

	deployment, err := createDeployment(app)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "app %q", app.Name)
	}

	// deployment will be nil if no deployment is generated and no error occurs,
	// so we only need to append this when a legit deployment resource is returned
	if deployment != nil {
		objects = append(objects, deployment)
		log.Debugf("app: %s, deployment: %s\n", app.Name, spew.Sprint(deployment))
	}
	objects = append(objects, configMap...)
	log.Debugf("app: %s, configMap: %s\n", app.Name, spew.Sprint(configMap))

	objects = append(objects, svcs...)
	log.Debugf("app: %s, service: %s\n", app.Name, spew.Sprint(svcs))

	objects = append(objects, ings...)
	log.Debugf("app: %s, ingress: %s\n", app.Name, spew.Sprint(ings))

	objects = append(objects, pvcs...)
	log.Debugf("app: %s, pvc: %s\n", app.Name, spew.Sprint(pvcs))

	objects = append(objects, secs...)
	log.Debugf("app: %s, secret: %s\n", app.Name, spew.Sprint(secs))

	return objects, app.ExtraResources, nil
}

// Transform function if given spec.App data creates the versioned
// kubernetes objects and returns them in list of runtime.Object
// And if the field in spec.App called 'extraResources' is used
// then it returns the filenames mentioned there as list of string
func Transform(app *spec.App) ([]runtime.Object, []string, error) {

	runtimeObjects, extraResources, err := CreateK8sObjects(app)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to create Kubernetes objects")
	}

	if len(runtimeObjects) == 0 {
		return nil, nil, errors.New("No runtime objects created, possibly because not enough input data was passed")
	}

	for _, runtimeObject := range runtimeObjects {

		gvk, isUnversioned, err := api.Scheme.ObjectKind(runtimeObject)
		if err != nil {
			return nil, nil, errors.Wrap(err, "ConvertToVersion failed")
		}
		if isUnversioned {
			return nil, nil, fmt.Errorf("ConvertToVersion failed: can't output unversioned type: %T", runtimeObject)
		}

		runtimeObject.GetObjectKind().SetGroupVersionKind(gvk)
	}

	return runtimeObjects, extraResources, nil
}
