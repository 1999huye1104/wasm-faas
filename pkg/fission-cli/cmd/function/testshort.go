package function

import (
	"context"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fission/fission/pkg/fission-cli/cliwrapper/cli"
	"github.com/fission/fission/pkg/fission-cli/cmd"
	"github.com/fission/fission/pkg/fission-cli/cmd/httptrigger"
	"github.com/fission/fission/pkg/fission-cli/console"
	flagkey "github.com/fission/fission/pkg/fission-cli/flag/key"
	"github.com/fission/fission/pkg/fission-cli/util"
	
)

type TestShortSubCommand struct {
	cmd.CommandActioner
}

func TestShort(input cli.Input) error {
	return (&TestShortSubCommand{}).do(input)
}

func (opts *TestShortSubCommand) do(input cli.Input) error {
	m := &metav1.ObjectMeta{
		Name:      input.String(flagkey.FnName),
		Namespace: input.String(flagkey.NamespaceFunction),
	}
	kubeContext := input.String(flagkey.KubeContext)
	routerURL := os.Getenv("FISSION_ROUTER")
	if len(routerURL) != 0 {
		console.Warn("The environment variable FISSION_ROUTER is no longer supported for this command")
	}

	// Portforward to the fission router
	localRouterPort, err := util.SetupPortForward(util.GetFissionNamespace(), "application=fission-agent", kubeContext)
	if err != nil {
		return err
	}
	fnURL := "http://127.0.0.1:" + localRouterPort + util.UrlForFunction(m.Name, m.Namespace)+"/"
	if input.IsSet(flagkey.FnSubPath) {
		subPath := input.String(flagkey.FnSubPath)
		if !strings.HasPrefix(subPath, "/") {
			fnURL = fnURL + "/" + subPath
		} else {
			fnURL = fnURL + subPath
		}
	}

	functionUrl, err := url.Parse(fnURL)
	if err != nil {
		return err
	}

	console.Verbose(2, "Function test url: %v", functionUrl.String())

	queryParams := input.StringSlice(flagkey.FnTestQuery)
	if len(queryParams) > 0 {
		query := url.Values{}
		for _, q := range queryParams {
			queryParts := strings.SplitN(q, "=", 2)
			var key, value string
			if len(queryParts) == 0 {
				continue
			}
			if len(queryParts) > 0 {
				key = queryParts[0]
			}
			if len(queryParts) > 1 {
				value = queryParts[1]
			}
			query.Set(key, value)
		}
		functionUrl.RawQuery = query.Encode()
	}

	var ctx context.Context

	testTimeout := input.Duration(flagkey.FnTestTimeout)
	if testTimeout <= 0*time.Second {
		ctx = context.Background()
	} else {
		var closeCtx context.CancelFunc
		ctx, closeCtx = context.WithTimeout(context.Background(), input.Duration(flagkey.FnTestTimeout))
		defer closeCtx()
	}

	methods := input.StringSlice(flagkey.HtMethod)
	if len(methods) == 0 {
		return errors.New("HTTP method not mentioned")
	} else if len(methods) > 1 {
		return errors.New("More than one HTTP method not supported")
	}
	method, err := httptrigger.GetMethod(methods[0])
	if err != nil {
		return err
	}
	resp, err := doHTTPRequest(ctx, functionUrl.String(),
		input.StringSlice(flagkey.FnTestHeader),
		method,
		input.String(flagkey.FnTestBody))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "error reading response from function")
	}

	if resp.StatusCode < 400 {
		os.Stdout.Write(body)
		result:=string(body)
		console.Info(result)
		return nil
	}

	console.Errorf("Error calling function %s: %d; Please try again or fix the error: %s\n", m.Name, resp.StatusCode, string(body))
	log, err := printPodLogs(opts.Client(), m)
	if err != nil {
		console.Errorf("Error getting function logs from controller: %v. Try to get logs from log database.", err)
		err = Log(input)
		if err != nil {
			return errors.Wrapf(err, "error retrieving function log from log database")
		}
	} else {
		console.Info(log)
	}
	return errors.New("error getting function response")
}

