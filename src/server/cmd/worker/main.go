package main

import (
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"path"
	"time"

	"golang.org/x/sync/errgroup"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/pkg/grpcutil"
	"github.com/pachyderm/pachyderm/src/client/pps"
	"github.com/pachyderm/pachyderm/src/client/version"
	"github.com/pachyderm/pachyderm/src/server/pkg/cmdutil"
	"github.com/pachyderm/pachyderm/src/server/pkg/ppsutil"
	"github.com/pachyderm/pachyderm/src/server/pkg/serviceenv"
	"github.com/pachyderm/pachyderm/src/server/worker"
	"google.golang.org/grpc"

	log "github.com/sirupsen/logrus"
)

// appEnv stores the environment variables that this worker needs
type appEnv struct {
	// Address of etcd, so that worker can write its own IP there for discoverh
	EtcdAddress string `env:"ETCD_PORT_2379_TCP_ADDR,required"`

	// Prefix in etcd for all pachd-related records
	PPSPrefix string `env:"PPS_ETCD_PREFIX,required"`

	// worker gets its own IP here, via the k8s downward API. It then writes that
	// IP back to etcd so that pachd can discover it
	PPSWorkerIP string `env:"PPS_WORKER_IP,required"`

	// The name of the pipeline that this worker belongs to
	PPSPipelineName string `env:"PPS_PIPELINE_NAME,required"`

	// The ID of the commit that contains the pipeline spec.
	PPSSpecCommitID string `env:"PPS_SPEC_COMMIT,required"`

	// The name of this pod
	PodName string `env:"PPS_POD_NAME,required"`

	// The namespace in which Pachyderm is deployed
	Namespace string `env:"PPS_NAMESPACE,required"`
}

func main() {
	cmdutil.Main(do, &appEnv{})
}

// getPipelineInfo gets the PipelineInfo proto describing the pipeline that this
// worker is part of.
// getPipelineInfo has the side effect of adding auth to the passed pachClient
// which is necessary to get the PipelineInfo from pfs.
func getPipelineInfo(env *serviceenv.ServiceEnv, pachClient *client.APIClient, appEnv *appEnv) (*pps.PipelineInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := env.GetEtcdClient().Get(ctx, path.Join(appEnv.PPSPrefix, "pipelines", appEnv.PPSPipelineName))
	if err != nil {
		return nil, err
	}
	if len(resp.Kvs) != 1 {
		return nil, fmt.Errorf("expected to find 1 pipeline (%s), got %d: %v", appEnv.PPSPipelineName, len(resp.Kvs), resp)
	}
	var pipelinePtr pps.EtcdPipelineInfo
	if err := pipelinePtr.Unmarshal(resp.Kvs[0].Value); err != nil {
		return nil, err
	}
	pachClient.SetAuthToken(pipelinePtr.AuthToken)
	// Notice we use the SpecCommitID from our env, not from etcd. This is
	// because the value in etcd might get updated while the worker pod is
	// being created and we don't want to run the transform of one version of
	// the pipeline in the image of a different verison.
	pipelinePtr.SpecCommit.ID = appEnv.PPSSpecCommitID
	return ppsutil.GetPipelineInfo(pachClient, &pipelinePtr)
}

func do(appEnvObj interface{}) error {
	go func() {
		log.Println(http.ListenAndServe(":651", nil))
	}()
	appEnv := appEnvObj.(*appEnv)

	// Construct a client that connects to the sidecar.
	env := serviceenv.InitServiceEnv("localhost:650", fmt.Sprintf("%s:2379", appEnv.EtcdAddress))
	pachClient := env.GetPachClient(context.Background())
	pipelineInfo, err := getPipelineInfo(env, pachClient, appEnv)
	if err != nil {
		return fmt.Errorf("error getting pipelineInfo: %v", err)
	}

	// Construct worker API server.
	workerRcName := ppsutil.PipelineRcName(pipelineInfo.Pipeline.Name, pipelineInfo.Version)
	apiServer, err := worker.NewAPIServer(env, pachClient, appEnv.PPSPrefix, pipelineInfo, appEnv.PodName, appEnv.Namespace)
	if err != nil {
		return err
	}

	// Start worker api server
	eg := errgroup.Group{}
	ready := make(chan error)
	eg.Go(func() error {
		return grpcutil.Serve(
			func(s *grpc.Server) {
				worker.RegisterWorkerServer(s, apiServer)
				close(ready)
			},
			grpcutil.ServeOptions{
				Version:    version.Version,
				MaxMsgSize: grpcutil.MaxMsgSize,
			},
			grpcutil.ServeEnv{
				GRPCPort: client.PPSWorkerPort,
			},
		)
	})

	// Wait until server is ready, then put our IP address into etcd, so pachd can
	// discover us
	<-ready
	key := path.Join(appEnv.PPSPrefix, "workers", workerRcName, appEnv.PPSWorkerIP)

	// Prepare to write "key" into etcd by creating lease -- if worker dies, our
	// IP will be removed from etcd
	ctx, cancel := context.WithTimeout(pachClient.Ctx(), 10*time.Second)
	defer cancel()
	resp, err := env.GetEtcdClient().Grant(ctx, 10 /* seconds */)
	if err != nil {
		return fmt.Errorf("error granting lease: %v", err)
	}
	// keepalive forever
	if _, err := env.GetEtcdClient().KeepAlive(context.Background(), resp.ID); err != nil {
		return fmt.Errorf("error with KeepAlive: %v", err)
	}

	// Actually write "key" into etcd
	if _, err := env.GetEtcdClient().Put(ctx, key, "", etcd.WithLease(resp.ID)); err != nil {
		return fmt.Errorf("error putting IP address: %v", err)
	}

	// If server ever exits, return error
	return eg.Wait()
}
