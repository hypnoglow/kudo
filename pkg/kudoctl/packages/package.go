package packages

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"regexp"
	"strconv"
	"strings"

	"github.com/kudobuilder/kudo/pkg/apis/kudo/v1alpha1"
	"github.com/kudobuilder/kudo/pkg/engine/task"
	"github.com/kudobuilder/kudo/pkg/kudoctl/files"
	"github.com/kudobuilder/kudo/pkg/util/kudo"

	"github.com/pkg/errors"
	"github.com/spf13/afero"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/yaml"
)

const (
	operatorFileName      = "operator.yaml"
	templateFileNameRegex = "templates/.*.yaml"
	paramsFileName        = "params.yaml"
)

const apiVersion = "kudo.dev/v1alpha1"

// PackageCRDs is collection of CRDs that are used when installing operator
// during installation, package format is converted to this structure
type PackageCRDs struct {
	Operator        *v1alpha1.Operator
	OperatorVersion *v1alpha1.OperatorVersion
	Instance        *v1alpha1.Instance
}

// PackageFiles represents the raw operator package format the way it is found in the tgz packages
type PackageFiles struct {
	Templates map[string]string
	Operator  *Operator
	Params    []v1alpha1.Parameter
}

// Operator is a representation of the KEP-9 Operator YAML
type Operator struct {
	Name              string                   `json:"name"`
	Description       string                   `json:"description,omitempty"`
	Version           string                   `json:"version"`
	AppVersion        string                   `json:"appVersion,omitempty"`
	KUDOVersion       string                   `json:"kudoVersion,omitempty"`
	KubernetesVersion string                   `json:"kubernetesVersion,omitempty"`
	Maintainers       []*v1alpha1.Maintainer   `json:"maintainers,omitempty"`
	URL               string                   `json:"url,omitempty"`
	Tasks             []v1alpha1.Task          `json:"tasks"`
	Plans             map[string]v1alpha1.Plan `json:"plans"`
}

// PackageFilesDigest is a tuple of data used to return the package files AND the digest of a tarball
type PackageFilesDigest struct {
	PkgFiles *PackageFiles
	Digest   string
}

func parsePackageFile(filePath string, fileBytes []byte, currentPackage *PackageFiles) error {
	isOperatorFile := func(name string) bool {
		return strings.HasSuffix(name, operatorFileName)
	}

	isTemplateFile := func(name string) bool {
		matched, err := regexp.Match(templateFileNameRegex, []byte(name))
		if err != nil {
			panic(err)
		}
		return matched
	}

	isParametersFile := func(name string) bool {
		return strings.HasSuffix(name, paramsFileName)
	}

	switch {
	case isOperatorFile(filePath):
		if err := yaml.Unmarshal(fileBytes, &currentPackage.Operator); err != nil {
			return errors.Wrap(err, "failed to unmarshal operator file")
		}
	case isTemplateFile(filePath):
		pathParts := strings.Split(filePath, "templates/")
		name := pathParts[len(pathParts)-1]
		currentPackage.Templates[name] = string(fileBytes)
	case isParametersFile(filePath):
		var params map[string]map[string]string
		if err := yaml.Unmarshal(fileBytes, &params); err != nil {
			return errors.Wrapf(err, "failed to unmarshal parameters file: %s", filePath)
		}
		paramsStruct := make([]v1alpha1.Parameter, 0)
		for paramName, param := range params {
			required := true // defaults to true
			if _, ok := param["required"]; ok {
				parsed, err := strconv.ParseBool(param["required"])
				if err != nil {
					// ideally this should never happen and be already caught by some kind of linter
					return errors.Wrapf(err, "failed parsing required field from parameter %s. cannot convert %s to bool", paramName, param["required"])
				}

				required = parsed
			}
			var defaultValue *string
			if val, ok := param["default"]; ok {
				defaultValue = kudo.String(val)
			}

			r := v1alpha1.Parameter{
				Name:        paramName,
				Description: param["description"],
				Default:     defaultValue,
				Trigger:     param["trigger"],
				Required:    required,
				DisplayName: param["displayName"],
			}
			paramsStruct = append(paramsStruct, r)
		}
		currentPackage.Params = paramsStruct
	default:
		return fmt.Errorf("unexpected file when reading package from filesystem: %s", filePath)
	}
	return nil
}

func newPackageFiles() PackageFiles {
	return PackageFiles{
		Templates: make(map[string]string),
	}
}

func validateTask(t v1alpha1.Task, templates map[string]string) []string {
	var resources []string
	switch t.Kind {
	case task.ApplyTaskKind:
		resources = t.Spec.ResourceTaskSpec.Resources
	case task.DeleteTaskKind:
		resources = t.Spec.ResourceTaskSpec.Resources
	case task.DummyTaskKind:
	default:
		log.Printf("no validation for task kind %s implemented", t.Kind)
	}

	var errs []string
	for _, res := range resources {
		if _, ok := templates[res]; !ok {
			errs = append(errs, fmt.Sprintf("task %s missing template: %s", t.Name, res))
		}
	}

	return errs
}

func (p *PackageFiles) getCRDs() (*PackageCRDs, error) {
	if p.Operator == nil {
		return nil, errors.New("operator.yaml file is missing")
	}
	if p.Params == nil {
		return nil, errors.New("params.yaml file is missing")
	}
	var errs []string
	for _, tt := range p.Operator.Tasks {
		errs = append(errs, validateTask(tt, p.Templates)...)
	}

	if len(errs) != 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}

	operator := &v1alpha1.Operator{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Operator",
			APIVersion: apiVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   p.Operator.Name,
			Labels: map[string]string{"controller-tools.k8s.io": "1.0"},
		},
		Spec: v1alpha1.OperatorSpec{
			Description:       p.Operator.Description,
			KudoVersion:       p.Operator.KUDOVersion,
			KubernetesVersion: p.Operator.KubernetesVersion,
			Maintainers:       p.Operator.Maintainers,
			URL:               p.Operator.URL,
		},
		Status: v1alpha1.OperatorStatus{},
	}

	fv := &v1alpha1.OperatorVersion{
		TypeMeta: metav1.TypeMeta{
			Kind:       "OperatorVersion",
			APIVersion: apiVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   fmt.Sprintf("%s-%s", p.Operator.Name, p.Operator.Version),
			Labels: map[string]string{"controller-tools.k8s.io": "1.0"},
		},
		Spec: v1alpha1.OperatorVersionSpec{
			Operator: v1.ObjectReference{
				Name: p.Operator.Name,
				Kind: "Operator",
			},
			Version:        p.Operator.Version,
			Templates:      p.Templates,
			Tasks:          p.Operator.Tasks,
			Parameters:     p.Params,
			Plans:          p.Operator.Plans,
			UpgradableFrom: nil,
		},
		Status: v1alpha1.OperatorVersionStatus{},
	}

	instance := &v1alpha1.Instance{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Instance",
			APIVersion: apiVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   fmt.Sprintf("%s-%s", p.Operator.Name, rand.String(6)),
			Labels: map[string]string{"controller-tools.k8s.io": "1.0", kudo.OperatorLabel: p.Operator.Name},
		},
		Spec: v1alpha1.InstanceSpec{
			OperatorVersion: v1.ObjectReference{
				Name: fmt.Sprintf("%s-%s", p.Operator.Name, p.Operator.Version),
			},
		},
		Status: v1alpha1.InstanceStatus{},
	}

	return &PackageCRDs{
		Operator:        operator,
		OperatorVersion: fv,
		Instance:        instance,
	}, nil
}

// GetFilesDigest maps []string of paths to the [] Operators
func GetFilesDigest(fs afero.Fs, paths []string) []*PackageFilesDigest {
	return mapPaths(fs, paths, pathToOperator)
}

// work of map path, swallows errors to return only packages that are valid
func mapPaths(fs afero.Fs, paths []string, f func(afero.Fs, string) (*PackageFilesDigest, error)) []*PackageFilesDigest {
	ops := make([]*PackageFilesDigest, 0)
	for _, path := range paths {
		op, err := f(fs, path)
		if err != nil {
			fmt.Printf("WARNING: operator: %v is invalid", path)
			continue
		}
		ops = append(ops, op)
	}

	return ops
}

// pathToOperator takes a single path and returns an operator or error
func pathToOperator(fs afero.Fs, path string) (pfd *PackageFilesDigest, err error) {
	reader, err := fs.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if ferr := reader.Close(); ferr != nil {
			err = ferr
		}
	}()

	digest, err := files.Sha256Sum(reader)
	if err != nil {
		return nil, err
	}
	// restart reading of file after getting digest
	_, err = reader.Seek(0, io.SeekStart)
	if err != nil {
		return nil, err
	}
	b, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	pkg, err := bufferToPackageFiles(bytes.NewBuffer(b))
	pfd = &PackageFilesDigest{
		pkg,
		digest,
	}
	return pfd, err
}

func bufferToPackageFiles(buf *bytes.Buffer) (*PackageFiles, error) {
	b := NewFromBytes(buf)
	pkg, err := b.GetPkgFiles()
	if err != nil {
		return nil, err
	}
	return pkg, nil
}
