package configuration

import (
	"fmt"
	"sync"
	"time"

	"github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/blobstore/local"
	"github.com/buildbarn/bb-storage/pkg/blobstore/mirrored"
	"github.com/buildbarn/bb-storage/pkg/blobstore/readcaching"
	"github.com/buildbarn/bb-storage/pkg/blobstore/readfallback"
	"github.com/buildbarn/bb-storage/pkg/blobstore/sharding"
	"github.com/buildbarn/bb-storage/pkg/blockdevice"
	"github.com/buildbarn/bb-storage/pkg/clock"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/filesystem"
	"github.com/buildbarn/bb-storage/pkg/grpc"
	pb "github.com/buildbarn/bb-storage/pkg/proto/configuration/blobstore"
	"github.com/buildbarn/bb-storage/pkg/random"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/go-redis/redis/extra/redisotel"
	"github.com/go-redis/redis/v8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// BlobAccessInfo contains an instance of BlobAccess and information
// relevant to its creation. It is returned by functions that construct
// BlobAccess instances, such as NewBlobAccessFromConfiguration().
type BlobAccessInfo struct {
	BlobAccess      blobstore.BlobAccess
	DigestKeyFormat digest.KeyFormat
}

func newRedisClient(opt *redis.Options) *redis.Client {
	client := redis.NewClient(opt)
	client.AddHook(redisotel.TracingHook{})
	return client
}

func newNestedBlobAccessBare(configuration *pb.BlobAccessConfiguration, creator BlobAccessCreator) (BlobAccessInfo, string, error) {
	readBufferFactory := creator.GetReadBufferFactory()
	storageTypeName := creator.GetStorageTypeName()
	switch backend := configuration.Backend.(type) {
	case *pb.BlobAccessConfiguration_Error:
		return BlobAccessInfo{
			BlobAccess:      blobstore.NewErrorBlobAccess(status.ErrorProto(backend.Error)),
			DigestKeyFormat: digest.KeyWithoutInstance,
		}, "error", nil
	case *pb.BlobAccessConfiguration_ReadCaching:
		slow, err := NewNestedBlobAccess(backend.ReadCaching.Slow, creator)
		if err != nil {
			return BlobAccessInfo{}, "", err
		}
		fast, err := NewNestedBlobAccess(backend.ReadCaching.Fast, creator)
		if err != nil {
			return BlobAccessInfo{}, "", err
		}
		replicator, err := NewBlobReplicatorFromConfiguration(backend.ReadCaching.Replicator, slow.BlobAccess, fast, creator)
		if err != nil {
			return BlobAccessInfo{}, "", err
		}
		return BlobAccessInfo{
			BlobAccess:      readcaching.NewReadCachingBlobAccess(slow.BlobAccess, fast.BlobAccess, replicator),
			DigestKeyFormat: slow.DigestKeyFormat,
		}, "read_caching", nil
	case *pb.BlobAccessConfiguration_Redis:
		tlsConfig, err := util.NewTLSConfigFromClientConfiguration(backend.Redis.Tls)
		if err != nil {
			return BlobAccessInfo{}, "", util.StatusWrap(err, "Failed to obtain TLS configuration")
		}

		var replicationTimeout time.Duration
		if backend.Redis.ReplicationTimeout != nil {
			if err := backend.Redis.ReplicationTimeout.CheckValid(); err != nil {
				return BlobAccessInfo{}, "", util.StatusWrap(err, "Failed to obtain replication timeout")
			}
			replicationTimeout = backend.Redis.ReplicationTimeout.AsDuration()
		}

		var dialTimeout time.Duration
		if backend.Redis.DialTimeout != nil {
			if err := backend.Redis.DialTimeout.CheckValid(); err != nil {
				return BlobAccessInfo{}, "", util.StatusWrap(err, "Failed to obtain dial timeout configuration")
			}
			dialTimeout = backend.Redis.DialTimeout.AsDuration()
		}

		var readTimeout time.Duration
		if backend.Redis.ReadTimeout != nil {
			if err := backend.Redis.ReadTimeout.CheckValid(); err != nil {
				return BlobAccessInfo{}, "", util.StatusWrap(err, "Failed to obtain read timeout configuration")
			}
			readTimeout = backend.Redis.ReadTimeout.AsDuration()
		}

		var writeTimeout time.Duration
		if backend.Redis.WriteTimeout != nil {
			if err := backend.Redis.WriteTimeout.CheckValid(); err != nil {
				return BlobAccessInfo{}, "", util.StatusWrap(err, "Failed to obtain write timeout configuration")
			}
			writeTimeout = backend.Redis.WriteTimeout.AsDuration()
		}

		var redisClient blobstore.RedisClient
		switch mode := backend.Redis.Mode.(type) {
		case *pb.RedisBlobAccessConfiguration_Clustered:
			// Gather retry configuration (min/max delay and overall retry attempts)
			minRetryDur := time.Millisecond * 32
			if mode.Clustered.MinimumRetryBackoff != nil {
				if err := mode.Clustered.MinimumRetryBackoff.CheckValid(); err != nil {
					return BlobAccessInfo{}, "", util.StatusWrap(err, "Failed to obtain minimum retry back off configuration")
				}
				minRetryDur = mode.Clustered.MinimumRetryBackoff.AsDuration()
			}

			maxRetryDur := time.Millisecond * 2048
			if mode.Clustered.MaximumRetryBackoff != nil {
				if err := mode.Clustered.MaximumRetryBackoff.CheckValid(); err != nil {
					return BlobAccessInfo{}, "", util.StatusWrap(err, "Failed to obtain maximum retry back off")
				}
				maxRetryDur = mode.Clustered.MaximumRetryBackoff.AsDuration()
			}

			maxRetries := 16 // Default will be 16
			if mode.Clustered.MaximumRetries != 0 {
				maxRetries = int(mode.Clustered.MaximumRetries)
			}

			redisClient = redis.NewClusterClient(
				&redis.ClusterOptions{
					Addrs:           mode.Clustered.Endpoints,
					TLSConfig:       tlsConfig,
					ReadOnly:        true,
					MaxRetries:      maxRetries,
					MinRetryBackoff: minRetryDur,
					MaxRetryBackoff: maxRetryDur,
					DialTimeout:     dialTimeout,
					ReadTimeout:     readTimeout,
					WriteTimeout:    writeTimeout,
					NewClient:       newRedisClient,
				})

		case *pb.RedisBlobAccessConfiguration_Single:
			redisClient = newRedisClient(
				&redis.Options{
					Addr:         mode.Single.Endpoint,
					Password:     mode.Single.Password,
					DB:           int(mode.Single.Db),
					TLSConfig:    tlsConfig,
					DialTimeout:  dialTimeout,
					ReadTimeout:  readTimeout,
					WriteTimeout: writeTimeout,
				})
		default:
			return BlobAccessInfo{}, "", status.Errorf(codes.InvalidArgument, "Redis configuration must either be clustered or single server")
		}

		digestKeyFormat := creator.GetBaseDigestKeyFormat()
		return BlobAccessInfo{
			BlobAccess: blobstore.NewRedisBlobAccess(
				redisClient,
				readBufferFactory,
				digestKeyFormat,
				backend.Redis.ReplicationCount,
				replicationTimeout),
			DigestKeyFormat: digestKeyFormat,
		}, "redis", nil
	case *pb.BlobAccessConfiguration_Remote:
		return BlobAccessInfo{
			BlobAccess:      blobstore.NewRemoteBlobAccess(backend.Remote.Address, storageTypeName, readBufferFactory),
			DigestKeyFormat: digest.KeyWithInstance,
		}, "remote", nil
	case *pb.BlobAccessConfiguration_Sharding:
		backends := make([]blobstore.BlobAccess, 0, len(backend.Sharding.Shards))
		weights := make([]uint32, 0, len(backend.Sharding.Shards))
		var combinedDigestKeyFormat *digest.KeyFormat
		for _, shard := range backend.Sharding.Shards {
			if shard.Backend == nil {
				// Drained backend.
				backends = append(backends, nil)
			} else {
				// Undrained backend.
				backend, err := NewNestedBlobAccess(shard.Backend, creator)
				if err != nil {
					return BlobAccessInfo{}, "", err
				}
				backends = append(backends, backend.BlobAccess)
				if combinedDigestKeyFormat == nil {
					combinedDigestKeyFormat = &backend.DigestKeyFormat
				} else {
					newDigestKeyFormat := combinedDigestKeyFormat.Combine(backend.DigestKeyFormat)
					combinedDigestKeyFormat = &newDigestKeyFormat
				}
			}

			if shard.Weight == 0 {
				return BlobAccessInfo{}, "", status.Errorf(codes.InvalidArgument, "Shards must have positive weights")
			}
			weights = append(weights, shard.Weight)
		}
		if combinedDigestKeyFormat == nil {
			return BlobAccessInfo{}, "", status.Errorf(codes.InvalidArgument, "Cannot create sharding blob access without any undrained backends")
		}
		return BlobAccessInfo{
			BlobAccess: sharding.NewShardingBlobAccess(
				backends,
				sharding.NewWeightedShardPermuter(weights),
				backend.Sharding.HashInitialization),
			DigestKeyFormat: *combinedDigestKeyFormat,
		}, "sharding", nil
	case *pb.BlobAccessConfiguration_SizeDistinguishing:
		small, err := NewNestedBlobAccess(backend.SizeDistinguishing.Small, creator)
		if err != nil {
			return BlobAccessInfo{}, "", err
		}
		large, err := NewNestedBlobAccess(backend.SizeDistinguishing.Large, creator)
		if err != nil {
			return BlobAccessInfo{}, "", err
		}
		return BlobAccessInfo{
			BlobAccess:      blobstore.NewSizeDistinguishingBlobAccess(small.BlobAccess, large.BlobAccess, backend.SizeDistinguishing.CutoffSizeBytes),
			DigestKeyFormat: small.DigestKeyFormat.Combine(large.DigestKeyFormat),
		}, "size_distinguishing", nil
	case *pb.BlobAccessConfiguration_Mirrored:
		backendA, err := NewNestedBlobAccess(backend.Mirrored.BackendA, creator)
		if err != nil {
			return BlobAccessInfo{}, "", err
		}
		backendB, err := NewNestedBlobAccess(backend.Mirrored.BackendB, creator)
		if err != nil {
			return BlobAccessInfo{}, "", err
		}
		replicatorAToB, err := NewBlobReplicatorFromConfiguration(backend.Mirrored.ReplicatorAToB, backendA.BlobAccess, backendB, creator)
		if err != nil {
			return BlobAccessInfo{}, "", err
		}
		replicatorBToA, err := NewBlobReplicatorFromConfiguration(backend.Mirrored.ReplicatorBToA, backendB.BlobAccess, backendA, creator)
		if err != nil {
			return BlobAccessInfo{}, "", err
		}
		return BlobAccessInfo{
			BlobAccess:      mirrored.NewMirroredBlobAccess(backendA.BlobAccess, backendB.BlobAccess, replicatorAToB, replicatorBToA),
			DigestKeyFormat: backendA.DigestKeyFormat.Combine(backendB.DigestKeyFormat),
		}, "mirrored", nil
	case *pb.BlobAccessConfiguration_Local:
		digestKeyFormat := creator.GetBaseDigestKeyFormat()
		persistent := backend.Local.Persistent

		// Create the backing store for blocks of data.
		var backendType string
		var sectorSizeBytes int
		var blockSectorCount int64
		var blockAllocator local.BlockAllocator
		dataSyncer := func() error { return nil }
		switch blocksBackend := backend.Local.BlocksBackend.(type) {
		case *pb.LocalBlobAccessConfiguration_BlocksInMemory_:
			backendType = "local_in_memory"
			// All data must be stored in memory. Because we
			// are not dealing with physical storage, there
			// is no need to take sector sizes into account.
			// Use a sector size of 1 byte to achieve
			// maximum storage density.
			sectorSizeBytes = 1
			blockSectorCount = blocksBackend.BlocksInMemory.BlockSizeBytes
			blockAllocator = local.NewInMemoryBlockAllocator(int(blocksBackend.BlocksInMemory.BlockSizeBytes))
		case *pb.LocalBlobAccessConfiguration_BlocksOnBlockDevice_:
			backendType = "local_block_device"
			// Data may be stored on a block device that is
			// memory mapped. Automatically determine the
			// block size based on the size of the block
			// device and the number of blocks.
			blocksOnBlockDevice := blocksBackend.BlocksOnBlockDevice
			var blockDevice blockdevice.BlockDevice
			var sectorCount int64
			var err error
			blockDevice, sectorSizeBytes, sectorCount, err = blockdevice.NewBlockDeviceFromConfiguration(
				blocksOnBlockDevice.Source,
				persistent == nil)
			if err != nil {
				return BlobAccessInfo{}, "", util.StatusWrap(err, "Failed to open blocks block device")
			}
			dataSyncer = blockDevice.Sync
			blockCount := blocksOnBlockDevice.SpareBlocks + backend.Local.OldBlocks + backend.Local.CurrentBlocks + backend.Local.NewBlocks
			blockSectorCount = sectorCount / int64(blockCount)

			cachedReadBufferFactory := readBufferFactory
			if cacheConfiguration := blocksOnBlockDevice.DataIntegrityValidationCache; cacheConfiguration != nil {
				dataIntegrityCheckingCache, err := digest.NewExistenceCacheFromConfiguration(cacheConfiguration, digestKeyFormat, "DataIntegrityValidationCache")
				if err != nil {
					return BlobAccessInfo{}, "", err
				}
				cachedReadBufferFactory = blobstore.NewValidationCachingReadBufferFactory(
					readBufferFactory,
					dataIntegrityCheckingCache)
			}

			blockAllocator = local.NewBlockDeviceBackedBlockAllocator(
				blockDevice,
				cachedReadBufferFactory,
				sectorSizeBytes,
				blockSectorCount,
				int(blockCount))
		default:
			return BlobAccessInfo{}, "", status.Error(codes.InvalidArgument, "Blocks backend not specified")
		}

		var globalLock sync.RWMutex
		var blockList local.BlockList
		var keyLocationMapHashInitialization uint64
		initialBlockCount := 0
		if persistent == nil {
			// Persistency is disabled. Provide a simple
			// volatile BlockList.
			blockList = local.NewVolatileBlockList(
				blockAllocator,
				sectorSizeBytes,
				blockSectorCount)
			keyLocationMapHashInitialization = random.CryptoThreadSafeGenerator.Uint64()
		} else {
			// Persistency is enabled. Reload previous
			// persistent state from disk.
			persistentStateDirectory, err := filesystem.NewLocalDirectory(persistent.StateDirectoryPath)
			if err != nil {
				return BlobAccessInfo{}, "", util.StatusWrap(err, "Failed to open persistent state directory")
			}
			persistentStateStore := local.NewDirectoryBackedPersistentStateStore(persistentStateDirectory)
			persistentState, err := persistentStateStore.ReadPersistentState()
			if err != nil {
				return BlobAccessInfo{}, "", util.StatusWrap(err, "Failed to reload persistent state")
			}
			keyLocationMapHashInitialization = persistentState.KeyLocationMapHashInitialization

			// Create a persistent BlockList. This will
			// attempt to reattach the old blocks. The
			// number of valid blocks is returned, so that
			// the dimensions of the OldNewCurrentLocationBlobMap
			// can be set properly.
			var persistentBlockList *local.PersistentBlockList
			persistentBlockList, initialBlockCount = local.NewPersistentBlockList(
				blockAllocator,
				sectorSizeBytes,
				blockSectorCount,
				persistentState.OldestEpochId,
				persistentState.Blocks)
			blockList = persistentBlockList

			// Start goroutines that update the persistent
			// state file when writes and block releases
			// occur.
			if err := persistent.MinimumEpochInterval.CheckValid(); err != nil {
				return BlobAccessInfo{}, "", util.StatusWrap(err, "Failed to obtain minimum epoch duration")
			}
			minimumEpochInterval := persistent.MinimumEpochInterval.AsDuration()
			periodicSyncer := local.NewPeriodicSyncer(
				persistentBlockList,
				&globalLock,
				persistentStateStore,
				clock.SystemClock,
				util.DefaultErrorLogger,
				10*time.Second,
				minimumEpochInterval,
				keyLocationMapHashInitialization,
				dataSyncer)
			go func() {
				for {
					periodicSyncer.ProcessBlockRelease()
				}
			}()
			go func() {
				for {
					periodicSyncer.ProcessBlockPut()
				}
			}()
		}

		locationBlobMap := local.NewOldCurrentNewLocationBlobMap(
			blockList,
			util.DefaultErrorLogger,
			storageTypeName,
			int64(sectorSizeBytes)*blockSectorCount,
			int(backend.Local.OldBlocks),
			int(backend.Local.CurrentBlocks),
			int(backend.Local.NewBlocks),
			initialBlockCount)

		// Create the backing store for the key-location map.
		var locationRecordArraySize int
		var locationRecordArray local.LocationRecordArray
		switch keyLocationMapBackend := backend.Local.KeyLocationMapBackend.(type) {
		case *pb.LocalBlobAccessConfiguration_KeyLocationMapInMemory_:
			locationRecordArraySize = int(keyLocationMapBackend.KeyLocationMapInMemory.Entries)
			locationRecordArray = local.NewInMemoryLocationRecordArray(
				locationRecordArraySize,
				locationBlobMap)
		case *pb.LocalBlobAccessConfiguration_KeyLocationMapOnBlockDevice:
			blockDevice, sectorSizeBytes, sectorCount, err := blockdevice.NewBlockDeviceFromConfiguration(
				keyLocationMapBackend.KeyLocationMapOnBlockDevice,
				persistent == nil)
			if err != nil {
				return BlobAccessInfo{}, "", util.StatusWrap(err, "Failed to open key-location map block device")
			}
			locationRecordArraySize = int((int64(sectorSizeBytes) * sectorCount) / local.BlockDeviceBackedLocationRecordSize)
			locationRecordArray = local.NewBlockDeviceBackedLocationRecordArray(
				blockDevice,
				locationBlobMap)
		default:
			return BlobAccessInfo{}, "", status.Errorf(codes.InvalidArgument, "Key-location map backend not specified")
		}

		return BlobAccessInfo{
			BlobAccess: local.NewKeyBlobMapBackedBlobAccess(
				local.NewLocationBasedKeyBlobMap(
					local.NewHashingKeyLocationMap(
						locationRecordArray,
						locationRecordArraySize,
						keyLocationMapHashInitialization,
						backend.Local.KeyLocationMapMaximumGetAttempts,
						int(backend.Local.KeyLocationMapMaximumPutAttempts),
						storageTypeName),
					locationBlobMap),
				digestKeyFormat,
				&globalLock,
				storageTypeName),
			DigestKeyFormat: digestKeyFormat,
		}, backendType, nil
	case *pb.BlobAccessConfiguration_ReadFallback:
		primary, err := NewNestedBlobAccess(backend.ReadFallback.Primary, creator)
		if err != nil {
			return BlobAccessInfo{}, "", err
		}
		secondary, err := NewNestedBlobAccess(backend.ReadFallback.Secondary, creator)
		if err != nil {
			return BlobAccessInfo{}, "", err
		}
		replicator, err := NewBlobReplicatorFromConfiguration(backend.ReadFallback.Replicator, secondary.BlobAccess, primary, creator)
		if err != nil {
			return BlobAccessInfo{}, "", err
		}
		return BlobAccessInfo{
			BlobAccess:      readfallback.NewReadFallbackBlobAccess(primary.BlobAccess, secondary.BlobAccess, replicator),
			DigestKeyFormat: primary.DigestKeyFormat.Combine(secondary.DigestKeyFormat),
		}, "read_fallback", nil
	case *pb.BlobAccessConfiguration_Demultiplexing:
		// Construct a trie for each of the backends specified
		// in the configuration indexed by instance name prefix.
		backendsTrie := digest.NewInstanceNameTrie()
		type demultiplexedBackendInfo struct {
			backend             blobstore.BlobAccess
			backendName         string
			instanceNamePatcher digest.InstanceNamePatcher
		}
		backends := make([]demultiplexedBackendInfo, 0, len(backend.Demultiplexing.InstanceNamePrefixes))
		for k, demultiplexed := range backend.Demultiplexing.InstanceNamePrefixes {
			matchInstanceNamePrefix, err := digest.NewInstanceName(k)
			if err != nil {
				return BlobAccessInfo{}, "", util.StatusWrapf(err, "Invalid instance name %#v", k)
			}
			addInstanceNamePrefix, err := digest.NewInstanceName(demultiplexed.AddInstanceNamePrefix)
			if err != nil {
				return BlobAccessInfo{}, "", util.StatusWrapf(err, "Invalid instance name %#v", demultiplexed.AddInstanceNamePrefix)
			}
			backend, err := NewNestedBlobAccess(demultiplexed.Backend, creator)
			if err != nil {
				return BlobAccessInfo{}, "", err
			}
			backendsTrie.Set(matchInstanceNamePrefix, len(backends))
			backends = append(backends, demultiplexedBackendInfo{
				backend:             backend.BlobAccess,
				backendName:         matchInstanceNamePrefix.String(),
				instanceNamePatcher: digest.NewInstanceNamePatcher(matchInstanceNamePrefix, addInstanceNamePrefix),
			})
		}
		return BlobAccessInfo{
			BlobAccess: blobstore.NewDemultiplexingBlobAccess(
				func(i digest.InstanceName) (blobstore.BlobAccess, string, digest.InstanceNamePatcher, error) {
					idx := backendsTrie.Get(i)
					if idx < 0 {
						return nil, "", digest.NoopInstanceNamePatcher, status.Errorf(codes.InvalidArgument, "Unknown instance name: %#v", i.String())
					}
					return backends[idx].backend, backends[idx].backendName, backends[idx].instanceNamePatcher, nil
				}),
			DigestKeyFormat: digest.KeyWithInstance,
		}, "demultiplexing", nil
	}
	return creator.NewCustomBlobAccess(configuration)
}

// NewNestedBlobAccess may be called by
// BlobAccessCreator.NewCustomBlobAccess() to create BlobAccess
// objects for instances nested inside the configuration.
func NewNestedBlobAccess(configuration *pb.BlobAccessConfiguration, creator BlobAccessCreator) (BlobAccessInfo, error) {
	if configuration == nil {
		return BlobAccessInfo{}, status.Error(codes.InvalidArgument, "Storage configuration not specified")
	}

	backend, backendType, err := newNestedBlobAccessBare(configuration, creator)
	if err != nil {
		return BlobAccessInfo{}, err
	}
	return BlobAccessInfo{
		BlobAccess:      blobstore.NewMetricsBlobAccess(backend.BlobAccess, clock.SystemClock, fmt.Sprintf("%s_%s", creator.GetStorageTypeName(), backendType)),
		DigestKeyFormat: backend.DigestKeyFormat,
	}, nil
}

// NewBlobAccessFromConfiguration creates a BlobAccess object based on a
// configuration file.
func NewBlobAccessFromConfiguration(configuration *pb.BlobAccessConfiguration, creator BlobAccessCreator) (BlobAccessInfo, error) {
	backend, err := NewNestedBlobAccess(configuration, creator)
	if err != nil {
		return BlobAccessInfo{}, err
	}
	return BlobAccessInfo{
		BlobAccess:      creator.WrapTopLevelBlobAccess(backend.BlobAccess),
		DigestKeyFormat: backend.DigestKeyFormat,
	}, nil
}

// NewCASAndACBlobAccessFromConfiguration is a convenience function to
// create BlobAccess objects for both the Content Addressable Storage
// and Action Cache. Most Buildbarn components tend to require access to
// both these data stores.
func NewCASAndACBlobAccessFromConfiguration(configuration *pb.BlobstoreConfiguration, grpcClientFactory grpc.ClientFactory, maximumMessageSizeBytes int) (blobstore.BlobAccess, blobstore.BlobAccess, error) {
	contentAddressableStorage, err := NewBlobAccessFromConfiguration(
		configuration.GetContentAddressableStorage(),
		NewCASBlobAccessCreator(grpcClientFactory, maximumMessageSizeBytes))
	if err != nil {
		return nil, nil, util.StatusWrap(err, "Failed to create Content Addressable Storage")
	}

	actionCache, err := NewBlobAccessFromConfiguration(
		configuration.GetActionCache(),
		NewACBlobAccessCreator(
			contentAddressableStorage,
			grpcClientFactory,
			maximumMessageSizeBytes))
	if err != nil {
		return nil, nil, util.StatusWrap(err, "Failed to create Action Cache")
	}

	return contentAddressableStorage.BlobAccess, actionCache.BlobAccess, nil
}
