package gc3

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Netflix/titus-executor/vpc/tool/container2"
	"github.com/hashicorp/go-multierror"

	"github.com/Netflix/titus-executor/logger"
	vpcapi "github.com/Netflix/titus-executor/vpc/api"
	"github.com/Netflix/titus-executor/vpc/tool/identity"
	"github.com/Netflix/titus-executor/vpc/tool/shared"
	"github.com/Netflix/titus-executor/vpc/tracehelpers"
	"github.com/pkg/errors"
	"go.opencensus.io/trace"
	"google.golang.org/grpc"
)

type Args struct {
	KubernetesPodsURL string
	MesosStateURL     string
	SourceOfTruth     string
}

func GC(ctx context.Context, timeout time.Duration, instanceIdentityProvider identity.InstanceIdentityProvider, conn *grpc.ClientConn, args Args) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ctx, span := trace.StartSpan(ctx, "GC")
	defer span.End()

	instanceIdentity, err := instanceIdentityProvider.GetIdentity(ctx)
	if err != nil {
		err = errors.Wrap(err, "Unable to get instance identity")
		span.SetStatus(trace.Status{
			Code:    trace.StatusCodeUnknown,
			Message: err.Error(),
		})
		return err
	}

	req := vpcapi.GCRequestV3{
		InstanceIdentity: instanceIdentity,
		Soft:             true,
	}

	switch args.SourceOfTruth {
	case "kubernetes":
		req.RunningTaskIDs, err = kubernetesTasks(ctx, args.KubernetesPodsURL)
	case "mesos":
		req.RunningTaskIDs, err = mesosTasks(ctx, args.MesosStateURL)
	default:
		err = fmt.Errorf("Source of truth %q unknown", args.SourceOfTruth)
	}

	if err != nil {
		err = errors.Wrap(err, "Could not fetch running tasks")
		tracehelpers.SetStatus(err, span)
		return err
	}
	logger.G(ctx).WithField("runningTasks", req.RunningTaskIDs).Debug("Found running tasks")

	client := vpcapi.NewTitusAgentVPCServiceClient(conn)

	resp, err := client.GCV3(ctx, &req)
	if err != nil {
		err = errors.Wrap(err, "Cannot call API to perform GC")
		tracehelpers.SetStatus(err, span)
		return err
	}

	if len(resp.RemovedAssignments) > 0 {
		logger.G(ctx).WithField("removedAssignments", resp.RemovedAssignments).Info("Recieved assignments to remove")
	}
	var result *multierror.Error
	for _, taskID := range resp.RemovedAssignments {
		logger.G(ctx).WithField("assignment", taskID).Info("Removing assignment")
		assignment, err := client.GetAssignment(ctx, &vpcapi.GetAssignmentRequest{
			TaskId: taskID,
		})
		if err != nil {
			result = multierror.Append(result, errors.Wrapf(err, "Unable to get assignment %s", taskID))
			continue
		}
		allocation := shared.AssignmentToAllocation(assignment.Assignment)
		err = container2.TeardownNetwork(ctx, allocation)
		if err != nil {
			result = multierror.Append(result, errors.Wrapf(err, "Unable to tear down network %s", taskID))
			continue
		}
		_, err = client.UnassignIPV3(ctx, &vpcapi.UnassignIPRequestV3{
			TaskId: taskID,
		})
		if err != nil {
			result = multierror.Append(result, errors.Wrapf(err, "Unable to unassign from vpc service %s", taskID))
		}
		logger.G(ctx).WithField("assignment", taskID).Info("Successfully removed assignment")

	}

	err = result.ErrorOrNil()
	if err != nil {
		logger.G(ctx).WithError(err).Error("Error removing assignments")
		tracehelpers.SetStatus(err, span)
		return err
	}
	return nil
}

func kubernetesTasks(ctx context.Context, url string) ([]string, error) {
	ctx, span := trace.StartSpan(ctx, "kubernetesTasks")
	defer span.End()
	body, err := shared.Get(ctx, url)
	if err != nil {
		err = errors.Wrap(err, "Could not fetch task body from Kubelet")
		tracehelpers.SetStatus(err, span)
		return nil, err
	}

	tasks, err := parseKubernetesTasksBody(body)
	if err != nil {
		tracehelpers.SetStatus(err, span)
		return nil, err
	}

	return tasks, nil
}

func parseKubernetesTasksBody(body []byte) ([]string, error) {
	podList, err := shared.ToPodList(body)
	if err != nil {
		err = errors.Wrap(err, "Could not decode body to podlist")
		return nil, err
	}

	ret := make([]string, len(podList.Items))
	for idx := range podList.Items {
		pod := podList.Items[idx]
		ret[idx] = shared.PodKey(&pod)
	}
	return ret, nil
}

func mesosTasks(ctx context.Context, url string) ([]string, error) {
	ctx, span := trace.StartSpan(ctx, "mesosTasks")
	defer span.End()
	body, err := shared.Get(ctx, url)
	if err != nil {
		err = errors.Wrap(err, "Could not fetch task body from Kubelet")
		tracehelpers.SetStatus(err, span)
		return nil, err
	}

	tasks, err := parseMesosTasksBody(body)
	if err != nil {
		tracehelpers.SetStatus(err, span)
		return nil, err
	}

	return tasks, nil
}

func parseMesosTasksBody(body []byte) ([]string, error) {
	var state State
	err := json.Unmarshal(body, &state)
	if err != nil {
		err = errors.Wrap(err, "Unable to unmarshal state")
		return nil, err
	}

	tasks := []string{}
	for _, framework := range state.Frameworks {
		for _, executor := range framework.Executors {
			for _, task := range executor.Tasks {
				// We consider all tasks by all running executors to be alive, even if they are in a terminal state
				// That is because the executor can hang on the task's network resources until it itself is terminated
				if task.Name == "" {
					return nil, fmt.Errorf("Invalid task: %v", task)
				}
				tasks = append(tasks, task.Name)
			}
		}
	}

	return tasks, nil
}
