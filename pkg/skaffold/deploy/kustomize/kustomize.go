/*
Copyright 2019 The Skaffold Authors

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

package kustomize

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/segmentio/textio"
	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/config"
	deployerr "github.com/GoogleContainerTools/skaffold/pkg/skaffold/deploy/error"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/deploy/kubectl"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/event"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/graph"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/kubernetes/manifest"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/output"
	latestV1 "github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/latest/v1"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/util"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/warnings"
)

var (
	DefaultKustomizePath = "."
	KustomizeFilePaths   = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}
	basePath             = "base"
	KustomizeBinaryCheck = kustomizeBinaryExists // For testing
)

// kustomization is the content of a kustomization.yaml file.
type kustomization struct {
	Components            []string              `yaml:"components"`
	Bases                 []string              `yaml:"bases"`
	Resources             []string              `yaml:"resources"`
	Patches               []patchWrapper        `yaml:"patches"`
	PatchesStrategicMerge []strategicMergePatch `yaml:"patchesStrategicMerge"`
	CRDs                  []string              `yaml:"crds"`
	PatchesJSON6902       []patchJSON6902       `yaml:"patchesJson6902"`
	ConfigMapGenerator    []configMapGenerator  `yaml:"configMapGenerator"`
	SecretGenerator       []secretGenerator     `yaml:"secretGenerator"`
}

type patchPath struct {
	Path  string `yaml:"path"`
	Patch string `yaml:"patch"`
}

type patchWrapper struct {
	*patchPath
}

type strategicMergePatch struct {
	Path  string
	Patch string
}

type patchJSON6902 struct {
	Path string `yaml:"path"`
}

type configMapGenerator struct {
	Files []string `yaml:"files"`
	Env   string   `yaml:"env"`
	Envs  []string `yaml:"envs"`
}

type secretGenerator struct {
	Files []string `yaml:"files"`
	Env   string   `yaml:"env"`
	Envs  []string `yaml:"envs"`
}

// Deployer deploys workflows using kustomize CLI.
type Deployer struct {
	*latestV1.KustomizeDeploy

	kubectl             kubectl.CLI
	insecureRegistries  map[string]bool
	labels              map[string]string
	globalConfig        string
	useKubectlKustomize bool
}

func NewDeployer(cfg kubectl.Config, labels map[string]string, d *latestV1.KustomizeDeploy) (*Deployer, error) {
	defaultNamespace := ""
	if d.DefaultNamespace != nil {
		var err error
		defaultNamespace, err = util.ExpandEnvTemplate(*d.DefaultNamespace, nil)
		if err != nil {
			return nil, err
		}
	}

	kubectl := kubectl.NewCLI(cfg, d.Flags, defaultNamespace)
	// if user has kustomize binary, prioritize that over kubectl kustomize
	useKubectlKustomize := !KustomizeBinaryCheck() && kubectlVersionCheck(kubectl)

	return &Deployer{
		KustomizeDeploy:     d,
		kubectl:             kubectl,
		insecureRegistries:  cfg.GetInsecureRegistries(),
		globalConfig:        cfg.GlobalConfig(),
		labels:              labels,
		useKubectlKustomize: useKubectlKustomize,
	}, nil
}

// Check for existence of kustomize binary in user's PATH
func kustomizeBinaryExists() bool {
	_, err := exec.LookPath("kustomize")

	return err == nil
}

// Check that kubectl version is valid to use kubectl kustomize
func kubectlVersionCheck(kubectl kubectl.CLI) bool {
	gt, err := kubectl.CompareVersionTo(context.Background(), 1, 14)
	if err != nil {
		return false
	}

	return gt == 1
}

// Deploy runs `kubectl apply` on the manifest generated by kustomize.
func (k *Deployer) Deploy(ctx context.Context, out io.Writer, builds []graph.Artifact) ([]string, error) {
	manifests, err := k.renderManifests(ctx, out, builds)
	if err != nil {
		return nil, err
	}

	if len(manifests) == 0 {
		return nil, nil
	}

	namespaces, err := manifests.CollectNamespaces()
	if err != nil {
		event.DeployInfoEvent(fmt.Errorf("could not fetch deployed resource namespace. "+
			"This might cause port-forward and deploy health-check to fail: %w", err))
	}

	if err := k.kubectl.WaitForDeletions(ctx, textio.NewPrefixWriter(out, " - "), manifests); err != nil {
		return nil, err
	}

	if err := k.kubectl.Apply(ctx, textio.NewPrefixWriter(out, " - "), manifests); err != nil {
		return nil, err
	}

	return namespaces, nil
}

func (k *Deployer) renderManifests(ctx context.Context, out io.Writer, builds []graph.Artifact) (manifest.ManifestList, error) {
	if err := k.kubectl.CheckVersion(ctx); err != nil {
		output.Default.Fprintln(out, "kubectl client version:", k.kubectl.Version(ctx))
		output.Default.Fprintln(out, err)
	}

	debugHelpersRegistry, err := config.GetDebugHelpersRegistry(k.globalConfig)
	if err != nil {
		return nil, deployerr.DebugHelperRetrieveErr(err)
	}

	manifests, err := k.readManifests(ctx)
	if err != nil {
		return nil, err
	}

	if len(manifests) == 0 {
		return nil, nil
	}

	manifests, err = manifests.ReplaceImages(builds)
	if err != nil {
		return nil, err
	}

	if manifests, err = manifest.ApplyTransforms(manifests, builds, k.insecureRegistries, debugHelpersRegistry); err != nil {
		return nil, err
	}

	return manifests.SetLabels(k.labels)
}

// Cleanup deletes what was deployed by calling Deploy.
func (k *Deployer) Cleanup(ctx context.Context, out io.Writer) error {
	manifests, err := k.readManifests(ctx)
	if err != nil {
		return err
	}

	if err := k.kubectl.Delete(ctx, textio.NewPrefixWriter(out, " - "), manifests); err != nil {
		return err
	}

	return nil
}

// Dependencies lists all the files that describe what needs to be deployed.
func (k *Deployer) Dependencies() ([]string, error) {
	deps := util.NewStringSet()
	for _, kustomizePath := range k.KustomizePaths {
		depsForKustomization, err := DependenciesForKustomization(kustomizePath)
		if err != nil {
			return nil, userErr(err)
		}
		deps.Insert(depsForKustomization...)
	}
	return deps.ToList(), nil
}

func (k *Deployer) Render(ctx context.Context, out io.Writer, builds []graph.Artifact, offline bool, filepath string) error {
	manifests, err := k.renderManifests(ctx, out, builds)
	if err != nil {
		return err
	}
	return manifest.Write(manifests.String(), filepath, out)
}

// Values of `patchesStrategicMerge` can be either:
// + a file path, referenced as a plain string
// + an inline patch referenced as a string literal
func (p *strategicMergePatch) UnmarshalYAML(node *yamlv3.Node) error {
	if node.Style == 0 || node.Style == yamlv3.DoubleQuotedStyle || node.Style == yamlv3.SingleQuotedStyle {
		p.Path = node.Value
	} else {
		p.Patch = node.Value
	}

	return nil
}

// UnmarshalYAML implements JSON unmarshalling by reading an inline yaml fragment.
func (p *patchWrapper) UnmarshalYAML(unmarshal func(interface{}) error) (err error) {
	pp := &patchPath{}
	if err := unmarshal(&pp); err != nil {
		var oldPathString string
		if err := unmarshal(&oldPathString); err != nil {
			return err
		}
		warnings.Printf("list of file paths deprecated: see https://github.com/kubernetes-sigs/kustomize/blob/master/docs/plugins/builtins.md#patchtransformer")
		pp.Path = oldPathString
	}
	p.patchPath = pp
	return nil
}

func pathExistsLocally(filename string, workingDir string) (bool, os.FileMode) {
	path := filename
	if !filepath.IsAbs(filename) {
		path = filepath.Join(workingDir, filename)
	}
	if f, err := os.Stat(path); err == nil {
		return true, f.Mode()
	}
	return false, 0
}

func (k *Deployer) readManifests(ctx context.Context) (manifest.ManifestList, error) {
	var manifests manifest.ManifestList
	for _, kustomizePath := range k.KustomizePaths {
		var out []byte
		var err error

		if k.useKubectlKustomize {
			out, err = k.kubectl.Kustomize(ctx, BuildCommandArgs(k.BuildArgs, kustomizePath))
		} else {
			cmd := exec.CommandContext(ctx, "kustomize", append([]string{"build"}, BuildCommandArgs(k.BuildArgs, kustomizePath)...)...)
			out, err = util.RunCmdOut(cmd)
		}

		if err != nil {
			return nil, userErr(err)
		}

		if len(out) == 0 {
			continue
		}
		manifests.Append(out)
	}
	return manifests, nil
}

func IsKustomizationBase(path string) bool {
	return filepath.Dir(path) == basePath
}

func IsKustomizationPath(path string) bool {
	filename := filepath.Base(path)
	for _, candidate := range KustomizeFilePaths {
		if filename == candidate {
			return true
		}
	}
	return false
}
