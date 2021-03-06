package cmd

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"sort"

	"github.com/ghodss/yaml"
	"github.com/go-openapi/spec"
	sp "github.com/linkerd/linkerd2/controller/gen/apis/serviceprofile/v1alpha1"
	"github.com/linkerd/linkerd2/pkg/profiles"
	"github.com/spf13/cobra"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

type templateConfig struct {
	ControlPlaneNamespace string
	ServiceNamespace      string
	ServiceName           string
	ClusterZone           string
}

var pathParamRegex = regexp.MustCompile(`\\{[^\}]*\\}`)

type profileOptions struct {
	name      string
	namespace string
	template  bool
	openAPI   string
}

func newProfileOptions() *profileOptions {
	return &profileOptions{
		name:      "",
		namespace: "default",
		template:  false,
		openAPI:   "",
	}
}

func (options *profileOptions) validate() error {
	outputs := 0
	if options.template {
		outputs++
	}
	if options.openAPI != "" {
		outputs++
	}
	if outputs != 1 {
		return errors.New("You must specify exactly one of --template or --open-api")
	}

	// a DNS-1035 label must consist of lower case alphanumeric characters or '-',
	// start with an alphabetic character, and end with an alphanumeric character
	if errs := validation.IsDNS1035Label(options.name); len(errs) != 0 {
		return fmt.Errorf("invalid service %q: %v", options.name, errs)
	}

	// a DNS-1123 label must consist of lower case alphanumeric characters or '-',
	// and must start and end with an alphanumeric character
	if errs := validation.IsDNS1123Label(options.namespace); len(errs) != 0 {
		return fmt.Errorf("invalid namespace %q: %v", options.namespace, errs)
	}

	return nil
}

func newCmdProfile() *cobra.Command {

	options := newProfileOptions()

	cmd := &cobra.Command{
		Use:   "profile [flags] (--template | --open-api file) (SERVICE)",
		Short: "Output service profile config for Kubernetes",
		Long: `Output service profile config for Kubernetes.

This outputs a service profile for the given service.

If the --template flag is specified, it outputs a service profile template.
Edit the template and then apply it with kubectl to add a service profile to
a service.

Example:
  linkerd profile -n emojivoto --template web-svc > web-svc-profile.yaml
  # (edit web-svc-profile.yaml manually)
  kubectl apply -f web-svc-profile.yaml

If the --open-api flag is specified, it reads the given OpenAPI
specification file and outputs a corresponding service profile.

Example:
  linkerd profile -n emojivoto --open-api web-svc.swagger web-svc | kubectl apply -f -`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			options.name = args[0]

			err := options.validate()
			if err != nil {
				return err
			}

			if options.template {
				return profiles.RenderProfileTemplate(options.namespace, options.name, controlPlaneNamespace, os.Stdout)
			} else if options.openAPI != "" {
				return renderOpenAPI(options, os.Stdout)
			}

			// we should never get here
			return errors.New("Unexpected error")
		},
	}

	cmd.PersistentFlags().BoolVar(&options.template, "template", options.template, "Output a service profile template")
	cmd.PersistentFlags().StringVar(&options.openAPI, "open-api", options.openAPI, "Output a service profile based on the given OpenAPI spec file")
	cmd.PersistentFlags().StringVarP(&options.namespace, "namespace", "n", options.namespace, "Namespace of the service")

	return cmd
}

func renderOpenAPI(options *profileOptions, w io.Writer) error {
	var input io.Reader
	if options.openAPI == "-" {
		input = os.Stdin
	} else {
		var err error
		input, err = os.Open(options.openAPI)
		if err != nil {
			return err
		}
	}

	bytes, err := ioutil.ReadAll(input)
	if err != nil {
		return fmt.Errorf("Error reading file: %s", err)
	}
	json, err := yaml.YAMLToJSON(bytes)
	if err != nil {
		return fmt.Errorf("Error parsing yaml: %s", err)
	}

	swagger := spec.Swagger{}
	err = swagger.UnmarshalJSON(json)
	if err != nil {
		return fmt.Errorf("Error parsing OpenAPI spec: %s", err)
	}

	profile := sp.ServiceProfile{
		ObjectMeta: meta_v1.ObjectMeta{
			Name:      fmt.Sprintf("%s.%s.svc.cluster.local", options.name, options.namespace),
			Namespace: controlPlaneNamespace,
		},
		TypeMeta: meta_v1.TypeMeta{
			APIVersion: "linkerd.io/v1alpha1",
			Kind:       "ServiceProfile",
		},
	}

	routes := make([]*sp.RouteSpec, 0)

	paths := make([]string, 0)
	if swagger.Paths != nil {
		for path := range swagger.Paths.Paths {
			paths = append(paths, path)
		}
		sort.Strings(paths)
	}

	for _, path := range paths {
		item := swagger.Paths.Paths[path]
		pathRegex := pathToRegex(path)
		if item.Delete != nil {
			spec := mkRouteSpec(path, pathRegex, http.MethodDelete, item.Delete.Responses)
			routes = append(routes, spec)
		}
		if item.Get != nil {
			spec := mkRouteSpec(path, pathRegex, http.MethodGet, item.Get.Responses)
			routes = append(routes, spec)
		}
		if item.Head != nil {
			spec := mkRouteSpec(path, pathRegex, http.MethodHead, item.Head.Responses)
			routes = append(routes, spec)
		}
		if item.Options != nil {
			spec := mkRouteSpec(path, pathRegex, http.MethodOptions, item.Options.Responses)
			routes = append(routes, spec)
		}
		if item.Patch != nil {
			spec := mkRouteSpec(path, pathRegex, http.MethodPatch, item.Patch.Responses)
			routes = append(routes, spec)
		}
		if item.Post != nil {
			spec := mkRouteSpec(path, pathRegex, http.MethodPost, item.Post.Responses)
			routes = append(routes, spec)
		}
		if item.Put != nil {
			spec := mkRouteSpec(path, pathRegex, http.MethodPut, item.Put.Responses)
			routes = append(routes, spec)
		}
	}

	profile.Spec.Routes = routes
	output, err := yaml.Marshal(profile)
	if err != nil {
		return fmt.Errorf("Error writing Service Profile: %s", err)
	}
	w.Write(output)

	return nil
}

func mkRouteSpec(path, pathRegex string, method string, responses *spec.Responses) *sp.RouteSpec {
	return &sp.RouteSpec{
		Name:            fmt.Sprintf("%s %s", method, path),
		Condition:       toReqMatch(pathRegex, method),
		ResponseClasses: toRspClasses(responses),
	}
}

func pathToRegex(path string) string {
	escaped := regexp.QuoteMeta(path)
	return pathParamRegex.ReplaceAllLiteralString(escaped, "[^/]*")
}

func toReqMatch(path string, method string) *sp.RequestMatch {
	return &sp.RequestMatch{
		PathRegex: path,
		Method:    method,
	}
}

func toRspClasses(responses *spec.Responses) []*sp.ResponseClass {
	if responses == nil {
		return nil
	}
	classes := make([]*sp.ResponseClass, 0)

	statuses := make([]int, 0)
	for status := range responses.StatusCodeResponses {
		statuses = append(statuses, status)
	}
	sort.Ints(statuses)

	for _, status := range statuses {
		cond := &sp.ResponseMatch{
			Status: &sp.Range{
				Min: uint32(status),
				Max: uint32(status),
			},
		}
		classes = append(classes, &sp.ResponseClass{
			Condition: cond,
			IsFailure: status >= 500,
		})
	}
	return classes
}
