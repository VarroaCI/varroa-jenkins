package controller

import (
	"fmt"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// JenkinsCR represents a jenkins.io/v1alpha2 Jenkins custom resource.
type JenkinsCR struct {
	APIVersion string        `json:"apiVersion" yaml:"apiVersion"`
	Kind       string        `json:"kind" yaml:"kind"`
	Metadata   JenkinsCRMeta `json:"metadata" yaml:"metadata"`
	Spec       JenkinsCRSpec `json:"spec" yaml:"spec"`
}

// JenkinsCRMeta is the metadata for a Jenkins CR.
type JenkinsCRMeta struct {
	Name      string `json:"name" yaml:"name"`
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
}

// JenkinsCRSpec is the spec for a Jenkins CR.
type JenkinsCRSpec struct {
	ConfigurationAsCode *JenkinsCRJCasC       `json:"configurationAsCode,omitempty" yaml:"configurationAsCode,omitempty"`
	Master              *JenkinsCRMaster      `json:"master" yaml:"master"`
	JenkinsAPISettings  *JenkinsCRAPISettings `json:"jenkinsAPISettings" yaml:"jenkinsAPISettings"`
	Backup              *JenkinsCRBackup      `json:"backup,omitempty" yaml:"backup,omitempty"`
}

// JenkinsCRJCasC references the JCasC ConfigMap.
type JenkinsCRJCasC struct {
	Configurations []JenkinsCRConfigMapRef `json:"configurations" yaml:"configurations"`
	Secret         JenkinsCRSecretRef      `json:"secret" yaml:"secret"`
}

// JenkinsCRConfigMapRef is a reference to a ConfigMap.
type JenkinsCRConfigMapRef struct {
	Name string `json:"name" yaml:"name"`
}

// JenkinsCRSecretRef is a reference to a Secret.
type JenkinsCRSecretRef struct {
	Name string `json:"name" yaml:"name"`
}

// JenkinsCRMaster defines the Jenkins master pod spec.
type JenkinsCRMaster struct {
	DisableCSRFProtection bool                 `json:"disableCSRFProtection" yaml:"disableCSRFProtection"`
	Containers            []JenkinsCRContainer `json:"containers,omitempty" yaml:"containers,omitempty"`
	Plugins               []JenkinsCRPlugin    `json:"plugins,omitempty" yaml:"plugins,omitempty"`
}

// JenkinsCRPlugin specifies a plugin to install.
type JenkinsCRPlugin struct {
	Name    string `json:"name" yaml:"name"`
	Version string `json:"version" yaml:"version"`
}

// JenkinsCRContainer defines a container in the master pod.
type JenkinsCRContainer struct {
	Name            string              `json:"name" yaml:"name"`
	Image           string              `json:"image" yaml:"image"`
	ImagePullPolicy string              `json:"imagePullPolicy" yaml:"imagePullPolicy"`
	Resources       *JenkinsCRResources `json:"resources,omitempty" yaml:"resources,omitempty"`
}

// JenkinsCRResources defines resource requests and limits.
type JenkinsCRResources struct {
	Requests map[string]string `json:"requests,omitempty" yaml:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty" yaml:"limits,omitempty"`
}

// JenkinsCRBackup defines backup configuration.
type JenkinsCRBackup struct {
	ContainerName               string          `json:"containerName" yaml:"containerName"`
	Interval                    int64           `json:"interval" yaml:"interval"`
	MakeBackupBeforePodDeletion bool            `json:"makeBackupBeforePodDeletion" yaml:"makeBackupBeforePodDeletion"`
	Action                      JenkinsCRAction `json:"action" yaml:"action"`
}

// JenkinsCRAction defines the backup action.
type JenkinsCRAction struct {
	Exec JenkinsCRExec `json:"exec" yaml:"exec"`
}

// JenkinsCRExec defines an exec-based action.
type JenkinsCRExec struct {
	Command []string `json:"command" yaml:"command"`
}

// JenkinsCRAPISettings configures how the operator accesses the Jenkins API.
type JenkinsCRAPISettings struct {
	AuthorizationStrategy string `json:"authorizationStrategy" yaml:"authorizationStrategy"`
}

// GenerateJenkinsCR creates a Jenkins CR from a Controller spec.
func GenerateJenkinsCR(cr *v1alpha1.Controller, configMapName string) (*JenkinsCR, error) {
	image := cr.Spec.Version
	if image == "" {
		image = "lts"
	}

	var plugins []JenkinsCRPlugin
	if cr.Spec.PluginSpec != nil {
		for _, p := range cr.Spec.PluginSpec.Entries {
			plugins = append(plugins, JenkinsCRPlugin{
				Name:    p.ArtifactId,
				Version: p.Version,
			})
		}
	}

	jc := &JenkinsCR{
		APIVersion: "jenkins.io/v1alpha2",
		Kind:       "Jenkins",
		Metadata: JenkinsCRMeta{
			Name:      controllerPrefix(cr) + "-jenkins",
			Namespace: cr.Namespace,
		},
		Spec: JenkinsCRSpec{
			ConfigurationAsCode: &JenkinsCRJCasC{
				Configurations: []JenkinsCRConfigMapRef{
					{Name: configMapName},
				},
				Secret: JenkinsCRSecretRef{
					Name: configMapName + "-secret",
				},
			},
			JenkinsAPISettings: &JenkinsCRAPISettings{
				AuthorizationStrategy: "loggedInUsersCanDoAnything",
			},
			Master: &JenkinsCRMaster{
				DisableCSRFProtection: true,
				Containers: []JenkinsCRContainer{
					{
						Name:            "jenkins-master",
						Image:           fmt.Sprintf("jenkins/jenkins:%s", image),
						ImagePullPolicy: "IfNotPresent",
						Resources: &JenkinsCRResources{
							Requests: map[string]string{
								"cpu":    "1",
								"memory": "600Mi",
							},
							Limits: map[string]string{
								"cpu":    "1500m",
								"memory": "3Gi",
							},
						},
					},
				},
				Plugins: plugins,
			},
		},
	}

	return jc, nil
}
