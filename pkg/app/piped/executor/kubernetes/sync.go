// Copyright 2020 The PipeCD Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kubernetes

import (
	"context"
	"time"

	provider "github.com/pipe-cd/pipe/pkg/app/piped/cloudprovider/kubernetes"
	"github.com/pipe-cd/pipe/pkg/model"
)

func (e *Executor) ensureSync(ctx context.Context, commitHash string, manifestsLoader func(ctx context.Context) ([]provider.Manifest, error)) model.StageStatus {
	// Load the manifests at the specified commit.
	e.LogPersister.AppendInfof("Loading manifests at commit %s for handling", commitHash)
	manifests, err := manifestsLoader(ctx)
	if err != nil {
		e.LogPersister.AppendErrorf("Failed while loading running manifests (%v)", err)
		return model.StageStatus_STAGE_FAILURE
	}
	e.LogPersister.AppendSuccessf("Successfully loaded %d manifests", len(manifests))

	// Because the loaded maninests are read-only
	// we duplicate them to avoid updating the shared manifests data in cache.
	manifests = duplicateManifests(manifests, "")

	// Add builtin annotations for tracking application live state.
	e.addBuiltinAnnontations(manifests, primaryVariant, commitHash)

	// Start applying all manifests to add or update running resources.
	if err := e.applyManifests(ctx, manifests); err != nil {
		return model.StageStatus_STAGE_FAILURE
	}

	if !e.config.Sync.Prune {
		e.LogPersister.AppendInfo("Resource GC was skipped because sync.prune was not configured")
		return model.StageStatus_STAGE_SUCCESS
	}

	// Wait for all applied manifests to be stable.
	// In theory, we don't need to wait for them to be stable before going to the next step
	// but waiting for a while reduces the number of Kubernetes changes in a short time.
	e.LogPersister.AppendInfo("Waiting for the applied manifests to be stable")
	select {
	case <-time.After(15 * time.Second):
		break
	case <-ctx.Done():
		break
	}

	// Find the running resources that are not defined in Git for removing.
	e.LogPersister.AppendInfo("Start finding all running resources but no longer defined in Git")
	liveResources, ok := e.AppLiveResourceLister.ListKubernetesResources()
	if !ok {
		e.LogPersister.AppendInfo("There is no data about live resource so no resource will be removed")
		return model.StageStatus_STAGE_SUCCESS
	}

	removeKeys := findRemoveResources(manifests, liveResources)
	if len(removeKeys) == 0 {
		e.LogPersister.AppendInfo("There are no live resources should be removed")
		return model.StageStatus_STAGE_SUCCESS
	}
	e.LogPersister.AppendInfof("Found %d live resources that are no longer defined in Git", len(removeKeys))

	// Start deleting all running resources that are not defined in Git.
	if err := e.deleteResources(ctx, removeKeys); err != nil {
		return model.StageStatus_STAGE_FAILURE
	}

	return model.StageStatus_STAGE_SUCCESS
}

func findRemoveResources(manifests []provider.Manifest, liveResources []provider.Manifest) []provider.ResourceKey {
	var (
		keys       = make(map[provider.ResourceKey]struct{}, len(manifests))
		removeKeys = make([]provider.ResourceKey, 0)
	)
	for _, m := range manifests {
		key := m.Key
		key.Namespace = ""
		keys[key] = struct{}{}
	}
	for _, m := range liveResources {
		key := m.Key
		key.Namespace = ""
		if _, ok := keys[key]; ok {
			continue
		}
		key.Namespace = m.Key.Namespace
		removeKeys = append(removeKeys, key)
	}
	return removeKeys
}
