package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/gin-gonic/gin"
	ginmiddleware "github.com/oapi-codegen/gin-middleware"
	"go.uber.org/zap"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"gpu-adapter/internal/adapter"
)

type Server struct {
	Adapter *adapter.Adapter
}

func Handler(a *adapter.Adapter, token string) (http.Handler, error) {
	spec, err := GetSwagger()
	if err != nil {
		return nil, err
	}
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.ContextWithFallback = true
	r.HandleMethodNotAllowed = true
	r.Use(func(c *gin.Context) {
		c.Next()
		if c.FullPath() == "/healthz" {
			return
		}
		fields := []zap.Field{zap.String("method", c.Request.Method), zap.String("path", c.Request.URL.Path), zap.Int("status", c.Writer.Status())}
		if len(c.Errors) > 0 {
			fields = append(fields, zap.String("error", c.Errors.String()))
		}
		zap.L().Info("request", fields...)
	}, gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, v any) {
		zap.L().Error("panic while handling a request", zap.Any("panic", v), zap.Stack("stack"))
		c.AbortWithStatus(http.StatusInternalServerError)
	}), func(c *gin.Context) {
		if c.Request.Method != http.MethodGet {
			c.Request = c.Request.WithContext(context.WithoutCancel(c.Request.Context()))
		}
	}, func(c *gin.Context) {
		if c.FullPath() != "/healthz" && subtle.ConstantTimeCompare([]byte(c.GetHeader("Authorization")), []byte("Bearer "+token)) != 1 {
			c.AbortWithStatus(http.StatusUnauthorized)
		}
	})
	fail := func(c *gin.Context, err error) {
		_ = c.Error(err)
		var e *adapter.Error
		switch {
		case errors.As(err, &e):
		case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
			e = &adapter.Error{Code: adapter.CodeTimeout, Message: err.Error()}
		default:
			e = &adapter.Error{Code: adapter.CodeKubernetes, Message: err.Error()}
		}
		body := Error{Code: ErrorCode(e.Code), Message: e.Message}
		if e.Busy != nil {
			for _, w := range e.Busy.Workloads {
				body.Busy.Workloads = append(body.Busy.Workloads, WorkloadRef{Type: WorkloadRefType(w.Type), Namespace: w.Namespace, Name: w.Name})
			}
		}
		c.JSON(http.StatusBadRequest, body)
	}
	validate := ginmiddleware.OapiRequestValidatorWithOptions(spec, &ginmiddleware.Options{
		Options: openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		MultiErrorHandler: func(me openapi3.MultiError) error {
			var lines []string
			for _, err := range me {
				lines = append(lines, strings.SplitN(err.Error(), "\n", 2)[0])
			}
			return errors.New(strings.Join(lines, "; "))
		},
		ErrorHandler: func(c *gin.Context, message string, _ int) {
			fail(c, &adapter.Error{Code: adapter.CodeInvalidSpec, Message: strings.TrimPrefix(message, "error in openapi3filter.RequestError: ")})
		},
	})
	RegisterHandlersWithOptions(r, NewStrictHandlerWithOptions(Server{a}, nil, StrictGinServerOptions{
		RequestErrorHandlerFunc: func(c *gin.Context, err error) {
			fail(c, &adapter.Error{Code: adapter.CodeInvalidSpec, Message: "cannot read request: " + err.Error()})
		},
		HandlerErrorFunc:         fail,
		ResponseErrorHandlerFunc: fail,
	}), GinServerOptions{
		Middlewares: []MiddlewareFunc{MiddlewareFunc(validate)},
		ErrorHandler: func(c *gin.Context, err error, _ int) {
			fail(c, &adapter.Error{Code: adapter.CodeInvalidSpec, Message: "cannot read request: " + err.Error()})
		},
	})
	return r, nil
}

func (s Server) Healthz(context.Context, HealthzRequestObject) (HealthzResponseObject, error) {
	return Healthz200Response{}, nil
}

func (s Server) ListGpus(ctx context.Context, _ ListGpusRequestObject) (ListGpusResponseObject, error) {
	gpus, err := s.Adapter.ListGPUs(ctx)
	if err != nil {
		return nil, err
	}
	out := ListGpus200JSONResponse{}
	for _, g := range gpus {
		out = append(out, gpu(g))
	}
	return out, nil
}

func (s Server) GetGpu(ctx context.Context, req GetGpuRequestObject) (GetGpuResponseObject, error) {
	g, err := s.Adapter.GetGPU(ctx, req.GpuRef)
	if err != nil {
		return nil, err
	}
	return GetGpu200JSONResponse{GpuJSONResponse(gpu(g))}, nil
}

func (s Server) ChangePartition(ctx context.Context, req ChangePartitionRequestObject) (ChangePartitionResponseObject, error) {
	b := req.Body
	g, err := s.Adapter.Partition(ctx, req.GpuRef, adapter.PartitionRequest{Mode: string(b.Mode), Delete: b.Delete, Create: b.Create, Force: b.Force})
	if err != nil {
		return nil, err
	}
	return ChangePartition200JSONResponse{GpuJSONResponse(gpu(g))}, nil
}

func (s Server) ListContainers(ctx context.Context, req ListContainersRequestObject) (ListContainersResponseObject, error) {
	list, err := s.Adapter.ListContainers(ctx, req.Params.Namespace, string(req.Params.State))
	if err != nil {
		return nil, err
	}
	out := ListContainers200JSONResponse{}
	for _, c := range list {
		out = append(out, container(c))
	}
	return out, nil
}

func (s Server) CreateContainer(ctx context.Context, req CreateContainerRequestObject) (CreateContainerResponseObject, error) {
	b := req.Body
	gpus, err := containerGPUs(b.Gpus)
	if err != nil {
		return nil, err
	}
	in := adapter.ContainerCreate{
		Common:  adapter.Common{Namespace: b.Namespace, Name: b.Name, Image: b.Image, CPUMillicores: int64(b.CpuMillicores), MemoryBytes: b.MemoryBytes, Ports: b.Ports},
		Kind:    string(b.Kind),
		GPUs:    gpus,
		Command: b.Command,
		Args:    b.Args,
	}
	if b.ImagePullAuth != (ImagePullAuth{}) {
		in.ImagePullAuth = &adapter.ImagePullAuth{Username: b.ImagePullAuth.Username, Password: b.ImagePullAuth.Password}
	}
	for _, e := range b.Env {
		in.Env = append(in.Env, adapter.EnvVar{Name: e.Name, Value: e.Value})
	}
	for _, v := range b.Volumes {
		in.Volumes = append(in.Volumes, adapter.ContainerVolume{Name: v.Name, MountPath: v.MountPath})
	}
	c, err := s.Adapter.CreateContainer(ctx, in)
	if err != nil {
		return nil, err
	}
	return CreateContainer201JSONResponse{ContainerJSONResponse(container(c))}, nil
}

func (s Server) GetContainer(ctx context.Context, req GetContainerRequestObject) (GetContainerResponseObject, error) {
	c, err := s.Adapter.GetContainer(ctx, req.Namespace, req.Name)
	if err != nil {
		return nil, err
	}
	return GetContainer200JSONResponse{ContainerJSONResponse(container(c))}, nil
}

func (s Server) DeleteContainer(ctx context.Context, req DeleteContainerRequestObject) (DeleteContainerResponseObject, error) {
	if err := s.Adapter.DeleteContainer(ctx, req.Namespace, req.Name); err != nil {
		return nil, err
	}
	return DeleteContainer204Response{}, nil
}

func (s Server) StartContainer(ctx context.Context, req StartContainerRequestObject) (StartContainerResponseObject, error) {
	gpus, err := containerGPUs(req.Body.Gpus)
	if err != nil {
		return nil, err
	}
	c, err := s.Adapter.StartContainer(ctx, req.Namespace, req.Name, gpus)
	if err != nil {
		return nil, err
	}
	return StartContainer200JSONResponse{ContainerJSONResponse(container(c))}, nil
}

func (s Server) ListVms(ctx context.Context, req ListVmsRequestObject) (ListVmsResponseObject, error) {
	list, err := s.Adapter.ListVMs(ctx, req.Params.Namespace, string(req.Params.State))
	if err != nil {
		return nil, err
	}
	out := ListVms200JSONResponse{}
	for _, v := range list {
		out = append(out, Vm{Namespace: v.Namespace, Name: v.Name, Status: WorkloadStatus{State: WorkloadState(v.Status.State), Reason: v.Status.Reason, Message: v.Status.Message}, Gpus: v.GPUs, Endpoint: v.Endpoint})
	}
	return out, nil
}

func (s Server) CreateVm(ctx context.Context, req CreateVmRequestObject) (CreateVmResponseObject, error) {
	b := req.Body
	in := adapter.VMCreate{
		Common:   adapter.Common{Namespace: b.Namespace, Name: b.Name, Image: b.Image, CPUMillicores: int64(b.CpuMillicores), MemoryBytes: b.MemoryBytes, Ports: b.Ports},
		GPUs:     b.Gpus,
		UserData: b.UserData,
	}
	for _, v := range b.Volumes {
		in.Volumes = append(in.Volumes, adapter.VMVolume{Name: v.Name})
	}
	v, err := s.Adapter.CreateVM(ctx, in)
	if err != nil {
		return nil, err
	}
	return CreateVm201JSONResponse{VmJSONResponse(Vm{Namespace: v.Namespace, Name: v.Name, Status: WorkloadStatus{State: WorkloadState(v.Status.State), Reason: v.Status.Reason, Message: v.Status.Message}, Gpus: v.GPUs, Endpoint: v.Endpoint})}, nil
}

func (s Server) GetVm(ctx context.Context, req GetVmRequestObject) (GetVmResponseObject, error) {
	v, err := s.Adapter.GetVM(ctx, req.Namespace, req.Name)
	if err != nil {
		return nil, err
	}
	return GetVm200JSONResponse{VmJSONResponse(Vm{Namespace: v.Namespace, Name: v.Name, Status: WorkloadStatus{State: WorkloadState(v.Status.State), Reason: v.Status.Reason, Message: v.Status.Message}, Gpus: v.GPUs, Endpoint: v.Endpoint})}, nil
}

func (s Server) DeleteVm(ctx context.Context, req DeleteVmRequestObject) (DeleteVmResponseObject, error) {
	if err := s.Adapter.DeleteVM(ctx, req.Namespace, req.Name); err != nil {
		return nil, err
	}
	return DeleteVm204Response{}, nil
}

func (s Server) StopVm(ctx context.Context, req StopVmRequestObject) (StopVmResponseObject, error) {
	v, err := s.Adapter.StopVM(ctx, req.Namespace, req.Name)
	if err != nil {
		return nil, err
	}
	return StopVm200JSONResponse{VmJSONResponse(Vm{Namespace: v.Namespace, Name: v.Name, Status: WorkloadStatus{State: WorkloadState(v.Status.State), Reason: v.Status.Reason, Message: v.Status.Message}, Gpus: v.GPUs, Endpoint: v.Endpoint})}, nil
}

func (s Server) UpdateVm(ctx context.Context, req UpdateVmRequestObject) (UpdateVmResponseObject, error) {
	v, err := s.Adapter.UpdateVM(ctx, req.Namespace, req.Name, req.Body.Gpus)
	if err != nil {
		return nil, err
	}
	return UpdateVm200JSONResponse{VmJSONResponse(Vm{Namespace: v.Namespace, Name: v.Name, Status: WorkloadStatus{State: WorkloadState(v.Status.State), Reason: v.Status.Reason, Message: v.Status.Message}, Gpus: v.GPUs, Endpoint: v.Endpoint})}, nil
}

func (s Server) StartVm(ctx context.Context, req StartVmRequestObject) (StartVmResponseObject, error) {
	v, err := s.Adapter.StartVM(ctx, req.Namespace, req.Name)
	if err != nil {
		return nil, err
	}
	return StartVm200JSONResponse{VmJSONResponse(Vm{Namespace: v.Namespace, Name: v.Name, Status: WorkloadStatus{State: WorkloadState(v.Status.State), Reason: v.Status.Reason, Message: v.Status.Message}, Gpus: v.GPUs, Endpoint: v.Endpoint})}, nil
}

func (s Server) CreateVolume(ctx context.Context, req CreateVolumeRequestObject) (CreateVolumeResponseObject, error) {
	v, err := s.Adapter.CreateVolume(ctx, adapter.Volume{Namespace: req.Body.Namespace, Name: req.Body.Name, SizeBytes: req.Body.SizeBytes})
	if err != nil {
		return nil, err
	}
	return CreateVolume201JSONResponse{Namespace: v.Namespace, Name: v.Name, SizeBytes: v.SizeBytes}, nil
}

func (s Server) ListVolumes(ctx context.Context, req ListVolumesRequestObject) (ListVolumesResponseObject, error) {
	list, err := s.Adapter.ListVolumes(ctx, req.Params.Namespace)
	if err != nil {
		return nil, err
	}
	out := ListVolumes200JSONResponse{}
	for _, v := range list {
		out = append(out, Volume{Namespace: v.Namespace, Name: v.Name, SizeBytes: v.SizeBytes})
	}
	return out, nil
}

func (s Server) DeleteVolume(ctx context.Context, req DeleteVolumeRequestObject) (DeleteVolumeResponseObject, error) {
	if err := s.Adapter.DeleteVolume(ctx, req.Namespace, req.Name); err != nil {
		return nil, err
	}
	return DeleteVolume204Response{}, nil
}

func gpu(g adapter.GPU) Gpu {
	out := Gpu{
		GpuId: g.GpuID, PciBusId: g.PciBusID, Model: g.Model, MemoryBytes: g.MemoryBytes, Binding: GpuBinding(g.Binding), Mode: PartitionMode(g.Mode),
		MigProfiles: []MigProfile{}, TimeSlicing: g.TimeSlicing, VmAttachable: g.VmAttachable,
	}
	for _, mi := range g.MigInstances {
		out.MigInstances = append(out.MigInstances, MigInstance{InstanceId: mi.InstanceID, Profile: mi.Profile})
	}
	for _, mp := range g.MigProfiles {
		out.MigProfiles = append(out.MigProfiles, MigProfile{Name: mp.Name, MemoryBytes: mp.MemoryBytes})
	}
	return out
}

func container(c adapter.Container) Container {
	out := Container{Namespace: c.Namespace, Name: c.Name, Kind: ContainerKind(c.Kind), Status: WorkloadStatus{State: WorkloadState(c.Status.State), Reason: c.Status.Reason, Message: c.Status.Message}, Gpus: []ContainerGpu{}, Endpoint: c.Endpoint}
	for _, g := range c.GPUs {
		var cg ContainerGpu
		switch g.Mode {
		case adapter.ModeFull:
			_ = cg.FromFullGpu(FullGpu{GpuId: g.GpuID})
		case adapter.ModeMIG:
			_ = cg.FromMigGpu(MigGpu{InstanceId: g.InstanceID})
		case adapter.ModeTimeSlicing:
			_ = cg.FromTimeSlicingGpu(TimeSlicingGpu{GpuId: g.GpuID, GpuMemoryBytes: g.GpuMemoryBytes})
		}
		out.Gpus = append(out.Gpus, cg)
	}
	return out
}

func containerGPUs(in []ContainerGpu) ([]adapter.ContainerGPU, error) {
	var out []adapter.ContainerGPU
	for _, cg := range in {
		v, err := cg.ValueByDiscriminator()
		if err != nil {
			return nil, &adapter.Error{Code: adapter.CodeInvalidSpec, Message: "gpus: " + err.Error()}
		}
		switch g := v.(type) {
		case FullGpu:
			out = append(out, adapter.ContainerGPU{Mode: adapter.ModeFull, GpuID: g.GpuId})
		case MigGpu:
			out = append(out, adapter.ContainerGPU{Mode: adapter.ModeMIG, InstanceID: g.InstanceId})
		case TimeSlicingGpu:
			out = append(out, adapter.ContainerGPU{Mode: adapter.ModeTimeSlicing, GpuID: g.GpuId, GpuMemoryBytes: g.GpuMemoryBytes})
		}
	}
	return out, nil
}
