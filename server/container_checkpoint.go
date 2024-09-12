package server

import (
	"errors"

	metadata "github.com/checkpoint-restore/checkpointctl/lib"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	types "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/cri-o/cri-o/internal/lib"
	"github.com/cri-o/cri-o/internal/log"
)

// CheckpointContainer checkpoints a container.
func (s *Server) CheckpointContainer(ctx context.Context, req *types.CheckpointContainerRequest) (*types.CheckpointContainerResponse, error) {
	if !s.config.RuntimeConfig.CheckpointRestore() {
		return nil, errors.New("checkpoint/restore support not available")
	}

	_, err := s.GetContainerFromShortID(ctx, req.ContainerId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "could not find container %q: %v", req.ContainerId, err)
	}

	config := &metadata.ContainerConfig{
		ID: req.ContainerId,
	}
	opts := &lib.ContainerCheckpointOptions{
		TargetFile:  req.Location,
		KeepRunning: true,
	}

	// Check if the request is for pre-copy checkpointing
	if req.PreCopy {
		log.Infof(ctx, "Initiating pre-copy checkpoint for container: %s", req.ContainerId)
		// Hardcoded for now
		opts.PreCopyIterations = 3
		opts.TrackMemoryChanges = true

		// Invoke the pre-copy specific checkpoint method
		if err := s.ContainerServer.PreCopyCheckpoint(ctx, config, opts); err != nil {
			return nil, err
		}
	} else {
		log.Infof(ctx, "Performing standard checkpoint for container: %s", req.ContainerId)
		_, err = s.ContainerServer.ContainerCheckpoint(ctx, config, opts)
		if err != nil {
			return nil, err
		}
	}

	log.Infof(ctx, "Checkpointed container: %s", req.ContainerId)
	return &types.CheckpointContainerResponse{}, nil
}
