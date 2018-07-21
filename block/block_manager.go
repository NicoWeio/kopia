package block

import (
	"bytes"
	"context"
	"crypto/aes"
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kopia/kopia/internal/kopialogging"
	"github.com/kopia/kopia/internal/packindex"
	"github.com/kopia/kopia/storage"
)

var log = kopialogging.Logger("kopia/block")
var formatLog = kopialogging.Logger("kopia/block/format")

// PackBlockPrefix is the prefix for all pack storage blocks.
const PackBlockPrefix = "p"

const (
	parallelFetches             = 5                // number of parallel reads goroutines
	flushPackIndexTimeout       = 10 * time.Minute // time after which all pending indexes are flushes
	newIndexBlockPrefix         = "n"
	defaultMinPreambleLength    = 32
	defaultMaxPreambleLength    = 32
	defaultPaddingUnit          = 4096
	autoCompactionMinBlockCount = 4 * parallelFetches
	autoCompactionMaxBlockCount = 64

	currentWriteVersion     = 1
	minSupportedReadVersion = 0
	maxSupportedReadVersion = currentWriteVersion

	indexLoadAttempts = 10
)

// Info is an information about a single block managed by Manager.
type Info = packindex.Info

// IndexInfo is an information about a single index block managed by Manager.
type IndexInfo struct {
	FileName  string
	Length    int64
	Timestamp time.Time
}

// Manager manages storage blocks at a low level with encryption, deduplication and packaging.
type Manager struct {
	Format FormattingOptions

	stats      Stats
	blockCache *blockCache
	listCache  *listCache
	st         storage.Storage

	mu                      sync.Mutex
	locked                  bool
	checkInvariantsOnUnlock bool

	currentPackItems      map[string]Info   // blocks that are in the pack block currently being built (all inline)
	currentPackDataLength int               // total length of all items in the current pack block
	packIndexBuilder      packindex.Builder // blocks that are in index currently being built (current pack and all packs saved but not committed)
	committedBlocks       *committedBlockIndex

	disableIndexFlushCount int
	flushPackIndexesAfter  time.Time // time when those indexes should be flushed

	closed chan struct{}

	writeFormatVersion int32 // format version to write

	maxPackSize int
	formatter   Formatter

	minPreambleLength int
	maxPreambleLength int
	paddingUnit       int
	timeNow           func() time.Time
}

// DeleteBlock marks the given blockID as deleted.
//
// NOTE: To avoid race conditions only blocks that cannot be possibly re-created
// should ever be deleted. That means that contents of such blocks should include some element
// of randomness or a contemporaneous timestamp that will never reappear.
func (bm *Manager) DeleteBlock(blockID string) error {
	bm.lock()
	defer bm.unlock()

	log.Debugf("DeleteBlock(%q)", blockID)

	// We have this block in current pack index and it's already deleted there.
	if bi, ok := bm.packIndexBuilder[blockID]; ok {
		if !bi.Deleted {
			if bi.PackFile == "" {
				// added and never committed, just forget about it.
				delete(bm.packIndexBuilder, blockID)
				delete(bm.currentPackItems, blockID)
				return nil
			}

			// added and committed.
			bi2 := *bi
			bi2.Deleted = true
			bi2.TimestampSeconds = bm.timeNow().Unix()
			bm.setPendingBlock(bi2)
		}
		return nil
	}

	// We have this block in current pack index and it's already deleted there.
	bi, err := bm.committedBlocks.getBlock(blockID)
	if err != nil {
		return err
	}

	if bi.Deleted {
		// already deleted
		return nil
	}

	// object present but not deleted, mark for deletion and add to pending
	bi2 := bi
	bi2.Deleted = true
	bi2.TimestampSeconds = bm.timeNow().Unix()
	bm.setPendingBlock(bi2)
	return nil
}

func (bm *Manager) setPendingBlock(i Info) {
	bm.packIndexBuilder.Add(i)
	bm.currentPackItems[i.BlockID] = i
}

func (bm *Manager) addToPackLocked(ctx context.Context, blockID string, data []byte, isDeleted bool) error {
	bm.assertLocked()

	data = cloneBytes(data)
	bm.currentPackDataLength += len(data)
	bm.setPendingBlock(Info{
		Deleted:          isDeleted,
		BlockID:          blockID,
		Payload:          data,
		Length:           uint32(len(data)),
		TimestampSeconds: bm.timeNow().Unix(),
	})

	if bm.currentPackDataLength >= bm.maxPackSize {
		if err := bm.finishPackAndMaybeFlushIndexesLocked(ctx); err != nil {
			return err
		}
	}

	return nil
}

func (bm *Manager) finishPackAndMaybeFlushIndexesLocked(ctx context.Context) error {
	bm.assertLocked()
	if err := bm.finishPackLocked(ctx); err != nil {
		return err
	}

	if bm.timeNow().After(bm.flushPackIndexesAfter) {
		if err := bm.flushPackIndexesLocked(ctx); err != nil {
			return err
		}
	}

	return nil
}

// Stats returns statistics about block manager operations.
func (bm *Manager) Stats() Stats {
	return bm.stats
}

// ResetStats resets statistics to zero values.
func (bm *Manager) ResetStats() {
	bm.stats = Stats{}
}

// DisableIndexFlush increments the counter preventing automatic index flushes.
func (bm *Manager) DisableIndexFlush() {
	bm.lock()
	defer bm.unlock()
	log.Debugf("DisableIndexFlush()")
	bm.disableIndexFlushCount++
}

// EnableIndexFlush decrements the counter preventing automatic index flushes.
// The flushes will be reenabled when the index drops to zero.
func (bm *Manager) EnableIndexFlush() {
	bm.lock()
	defer bm.unlock()
	log.Debugf("EnableIndexFlush()")
	bm.disableIndexFlushCount--
}

func (bm *Manager) verifyInvariantsLocked() {
	bm.assertLocked()

	bm.verifyCurrentPackItemsLocked()
	bm.verifyPackIndexBuilderLocked()
}

func (bm *Manager) verifyCurrentPackItemsLocked() {
	for k, cpi := range bm.currentPackItems {
		if cpi.BlockID != k {
			bm.invariantViolated("block ID entry has invalid key: %v %v", cpi.BlockID, k)
		}
		if cpi.PackFile != "" && !cpi.Deleted {
			bm.invariantViolated("block ID entry has unexpected pack block ID %v: %v", cpi.BlockID, cpi.PackFile)
		}
		if cpi.TimestampSeconds == 0 {
			bm.invariantViolated("block has no timestamp: %v", cpi.BlockID)
		}
		bi, ok := bm.packIndexBuilder[k]
		if !ok {
			bm.invariantViolated("block ID entry not present in pack index builder: %v", cpi.BlockID)
		}
		if !reflect.DeepEqual(*bi, cpi) {
			bm.invariantViolated("current pack index does not match pack index builder: %v", cpi, *bi)
		}
	}
}

func (bm *Manager) verifyPackIndexBuilderLocked() {
	for k, cpi := range bm.packIndexBuilder {
		if cpi.BlockID != k {
			bm.invariantViolated("block ID entry has invalid key: %v %v", cpi.BlockID, k)
		}
		if _, ok := bm.currentPackItems[cpi.BlockID]; ok {
			// ignore blocks also in currentPackItems
			continue
		}
		if cpi.Deleted {
			if cpi.PackFile != "" {
				bm.invariantViolated("block can't be both deleted and have a pack block: %v", cpi.BlockID)
			}
		} else {
			if cpi.PackFile == "" {
				bm.invariantViolated("block that's not deleted must have a pack block: %+v", cpi)
			}
			if cpi.FormatVersion != byte(bm.writeFormatVersion) {
				bm.invariantViolated("block that's not deleted must have a valid format version: %+v", cpi)
			}
		}
		if cpi.TimestampSeconds == 0 {
			bm.invariantViolated("block has no timestamp: %v", cpi.BlockID)
		}
	}
}

func (bm *Manager) invariantViolated(msg string, arg ...interface{}) {
	if len(arg) > 0 {
		msg = fmt.Sprintf(msg, arg...)
	}

	panic(msg)
}

func (bm *Manager) startPackIndexLocked() {
	bm.currentPackItems = make(map[string]Info)
	bm.currentPackDataLength = 0
}

func (bm *Manager) flushPackIndexesLocked(ctx context.Context) error {
	bm.assertLocked()

	if bm.disableIndexFlushCount > 0 {
		log.Debugf("not flushing index because flushes are currently disabled")
		return nil
	}

	if len(bm.packIndexBuilder) > 0 {
		var buf bytes.Buffer

		if err := bm.packIndexBuilder.Build(&buf); err != nil {
			return fmt.Errorf("unable to build pack index: %v", err)
		}

		data := buf.Bytes()
		dataCopy := append([]byte(nil), data...)

		indexBlockID, err := bm.writePackIndexesNew(ctx, data)
		if err != nil {
			return err
		}

		if err := bm.committedBlocks.addBlock(indexBlockID, dataCopy, true); err != nil {
			return fmt.Errorf("unable to add committed block: %v", err)
		}
		bm.packIndexBuilder = packindex.NewBuilder()
	}

	bm.flushPackIndexesAfter = bm.timeNow().Add(flushPackIndexTimeout)
	return nil
}

func (bm *Manager) writePackIndexesNew(ctx context.Context, data []byte) (string, error) {
	return bm.encryptAndWriteBlockNotLocked(ctx, data, newIndexBlockPrefix)
}

func (bm *Manager) finishPackLocked(ctx context.Context) error {
	if len(bm.currentPackItems) == 0 {
		log.Debugf("no current pack entries")
		return nil
	}

	if err := bm.writePackBlockLocked(ctx); err != nil {
		return fmt.Errorf("error writing pack block: %v", err)
	}

	bm.startPackIndexLocked()
	return nil
}

func (bm *Manager) writePackBlockLocked(ctx context.Context) error {
	bm.assertLocked()

	blockData, pending, err := bm.preparePackDataBlock()
	if err != nil {
		return fmt.Errorf("error preparing data block: %v", err)
	}

	packFile, err := bm.writePackDataNotLocked(ctx, blockData)
	if err != nil {
		return fmt.Errorf("can't save pack data block: %v", err)
	}

	formatLog.Debugf("wrote pack file: %v", packFile)

	for _, info := range pending {
		info.PackFile = packFile
		bm.packIndexBuilder.Add(info)
	}

	return nil
}

func (bm *Manager) preparePackDataBlock() ([]byte, map[string]Info, error) {
	formatLog.Debugf("preparing block data with %v items", len(bm.currentPackItems))
	blockData, err := appendRandomBytes(nil, rand.Intn(bm.maxPreambleLength-bm.minPreambleLength+1)+bm.minPreambleLength)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to prepare block preamble: %v", err)
	}

	pending := map[string]Info{}
	for blockID, info := range bm.currentPackItems {
		if info.Payload == nil {
			continue
		}
		var encrypted []byte
		encrypted, err = bm.maybeEncryptBlockDataForPacking(info.Payload, info.BlockID)
		if err != nil {
			return nil, nil, fmt.Errorf("unable to encrypt %q: %v", blockID, err)
		}

		formatLog.Debugf("adding %v length=%v deleted=%v", blockID, len(info.Payload), info.Deleted)

		pending[blockID] = Info{
			BlockID:          blockID,
			Deleted:          info.Deleted,
			FormatVersion:    byte(bm.writeFormatVersion),
			PackOffset:       uint32(len(blockData)),
			Length:           uint32(len(info.Payload)),
			TimestampSeconds: info.TimestampSeconds,
		}

		blockData = append(blockData, encrypted...)
	}

	if len(pending) == 0 {
		return nil, nil, nil
	}

	if bm.paddingUnit > 0 {
		if missing := bm.paddingUnit - (len(blockData) % bm.paddingUnit); missing > 0 {
			blockData, err = appendRandomBytes(blockData, missing)
			if err != nil {
				return nil, nil, fmt.Errorf("unable to prepare block postamble: %v", err)
			}
		}
	}
	formatLog.Debugf("finished block %v bytes", len(blockData))

	return blockData, pending, nil

}

func (bm *Manager) maybeEncryptBlockDataForPacking(data []byte, blockID string) ([]byte, error) {
	if bm.writeFormatVersion == 0 {
		// in v0 the entire block is encrypted together later on
		return data, nil
	}
	iv, err := getPackedBlockIV(blockID)
	if err != nil {
		return nil, fmt.Errorf("unable to get packed block IV for %q: %v", blockID, err)
	}
	return bm.formatter.Encrypt(data, iv)
}

func appendRandomBytes(b []byte, count int) ([]byte, error) {
	rnd := make([]byte, count)
	if _, err := io.ReadFull(cryptorand.Reader, rnd); err != nil {
		return nil, err
	}

	return append(b, rnd...), nil
}

// IndexBlocks returns the list of active index blocks.
func (bm *Manager) IndexBlocks(ctx context.Context) ([]IndexInfo, error) {
	return bm.listCache.listIndexBlocks(ctx)
}

func (bm *Manager) loadPackIndexesUnlocked(ctx context.Context) ([]IndexInfo, bool, error) {
	nextSleepTime := 100 * time.Millisecond

	for i := 0; i < indexLoadAttempts; i++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}

		if i > 0 {
			bm.listCache.deleteListCache(ctx)
			log.Debugf("encountered NOT_FOUND when loading, sleeping %v before retrying #%v", nextSleepTime, i)
			time.Sleep(nextSleepTime)
			nextSleepTime *= 2
		}

		blocks, err := bm.listCache.listIndexBlocks(ctx)
		if err != nil {
			return nil, false, err
		}

		err = bm.tryLoadPackIndexBlocksUnlocked(ctx, blocks)
		if err == nil {
			var blockIDs []string
			for _, b := range blocks {
				blockIDs = append(blockIDs, b.FileName)
			}
			var updated bool
			updated, err = bm.committedBlocks.use(blockIDs)
			if err != nil {
				return nil, false, err
			}
			return blocks, updated, nil
		}
		if err != storage.ErrBlockNotFound {
			return nil, false, err
		}
	}

	return nil, false, fmt.Errorf("unable to load pack indexes despite %v retries", indexLoadAttempts)
}

func (bm *Manager) tryLoadPackIndexBlocksUnlocked(ctx context.Context, blocks []IndexInfo) error {
	ch, err := bm.unprocessedIndexBlocksUnlocked(blocks)
	if err != nil {
		return err
	}
	if len(ch) == 0 {
		return nil
	}

	var wg sync.WaitGroup

	errors := make(chan error, parallelFetches)

	for i := 0; i < parallelFetches; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for indexBlockID := range ch {
				data, err := bm.getPhysicalBlockInternal(ctx, indexBlockID)
				if err != nil {
					errors <- err
					return
				}

				if err := bm.committedBlocks.addBlock(indexBlockID, data, false); err != nil {
					errors <- fmt.Errorf("unable to add to committed block cache: %v", err)
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errors)

	// Propagate async errors, if any.
	for err := range errors {
		return err
	}

	return nil
}

// unprocessedIndexBlocksUnlocked returns a closed channel filled with block IDs that are not in committedBlocks cache.
func (bm *Manager) unprocessedIndexBlocksUnlocked(blocks []IndexInfo) (<-chan string, error) {
	ch := make(chan string, len(blocks))
	for _, block := range blocks {
		has, err := bm.committedBlocks.cache.hasIndexBlockID(block.FileName)
		if err != nil {
			return nil, err
		}
		if has {
			log.Debugf("index block %q already in cache, skipping", block.FileName)
			continue
		}
		ch <- block.FileName
	}
	close(ch)
	return ch, nil
}

// Close closes the block manager.
func (bm *Manager) Close() {
	bm.blockCache.close()
	close(bm.closed)
}

// CompactIndexes performs compaction of index blocks ensuring that # of small blocks is between minSmallBlockCount and maxSmallBlockCount
func (bm *Manager) CompactIndexes(ctx context.Context, minSmallBlockCount int, maxSmallBlockCount int) error {
	log.Debugf("CompactIndexes(%v,%v)", minSmallBlockCount, maxSmallBlockCount)
	if maxSmallBlockCount < minSmallBlockCount {
		return fmt.Errorf("invalid block counts")
	}

	indexBlocks, _, err := bm.loadPackIndexesUnlocked(ctx)
	if err != nil {
		return fmt.Errorf("error loading indexes: %v", err)
	}

	blocksToCompact := bm.getBlocksToCompact(indexBlocks, minSmallBlockCount, maxSmallBlockCount)

	if err := bm.compactAndDeleteIndexBlocks(ctx, blocksToCompact); err != nil {
		log.Warningf("error performing quick compaction: %v", err)
	}

	return nil
}

func (bm *Manager) getBlocksToCompact(indexBlocks []IndexInfo, minSmallBlockCount int, maxSmallBlockCount int) []IndexInfo {
	var nonCompactedBlocks []IndexInfo
	var totalSizeNonCompactedBlocks int64

	var verySmallBlocks []IndexInfo
	var totalSizeVerySmallBlocks int64

	var mediumSizedBlocks []IndexInfo
	var totalSizeMediumSizedBlocks int64

	for _, b := range indexBlocks {
		if b.Length > int64(bm.maxPackSize) {
			continue
		}

		nonCompactedBlocks = append(nonCompactedBlocks, b)
		if b.Length < int64(bm.maxPackSize/20) {
			verySmallBlocks = append(verySmallBlocks, b)
			totalSizeVerySmallBlocks += b.Length
		} else {
			mediumSizedBlocks = append(mediumSizedBlocks, b)
			totalSizeMediumSizedBlocks += b.Length
		}
		totalSizeNonCompactedBlocks += b.Length
	}

	if len(nonCompactedBlocks) < minSmallBlockCount {
		// current count is below min allowed - nothing to do
		formatLog.Debugf("no small blocks to compact")
		return nil
	}

	if len(verySmallBlocks) > len(nonCompactedBlocks)/2 && len(mediumSizedBlocks)+1 < minSmallBlockCount {
		formatLog.Debugf("compacting %v very small blocks", len(verySmallBlocks))
		return verySmallBlocks
	}

	formatLog.Debugf("compacting all %v non-compacted blocks", len(nonCompactedBlocks))
	return nonCompactedBlocks
}

// ListBlocks returns IDs of blocks matching given prefix.
func (bm *Manager) ListBlocks(prefix string) ([]string, error) {
	bm.lock()
	defer bm.unlock()

	var result []string

	appendToResult := func(i Info) error {
		if i.Deleted || !strings.HasPrefix(i.BlockID, prefix) {
			return nil
		}
		if bi, ok := bm.packIndexBuilder[i.BlockID]; ok && bi.Deleted {
			return nil
		}
		result = append(result, i.BlockID)
		return nil
	}

	for _, bi := range bm.packIndexBuilder {
		_ = appendToResult(*bi)
	}

	_ = bm.committedBlocks.listBlocks(prefix, appendToResult)
	return result, nil
}

// ListBlockInfos returns the metadata about blocks with a given prefix and kind.
func (bm *Manager) ListBlockInfos(prefix string, includeDeleted bool) ([]Info, error) {
	bm.lock()
	defer bm.unlock()

	var result []Info

	appendToResult := func(i Info) error {
		if (i.Deleted && !includeDeleted) || !strings.HasPrefix(i.BlockID, prefix) {
			return nil
		}
		if bi, ok := bm.packIndexBuilder[i.BlockID]; ok && bi.Deleted {
			return nil
		}
		result = append(result, i)
		return nil
	}

	for _, bi := range bm.packIndexBuilder {
		_ = appendToResult(*bi)
	}

	_ = bm.committedBlocks.listBlocks(prefix, appendToResult)

	return result, nil
}

func (bm *Manager) compactAndDeleteIndexBlocks(ctx context.Context, indexBlocks []IndexInfo) error {
	if len(indexBlocks) <= 1 {
		return nil
	}
	formatLog.Debugf("compacting %v blocks", len(indexBlocks))
	t0 := time.Now()

	bld := packindex.NewBuilder()
	for _, indexBlock := range indexBlocks {
		data, err := bm.getPhysicalBlockInternal(ctx, indexBlock.FileName)
		if err != nil {
			return err
		}

		index, err := packindex.Open(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("unable to open index block %q: %v", indexBlock, err)
		}

		_ = index.Iterate("", func(i Info) error {
			bld.Add(i)
			return nil
		})
	}

	var buf bytes.Buffer
	if err := bld.Build(&buf); err != nil {
		return fmt.Errorf("unable to build an index: %v", err)
	}

	compactedIndexBlock, err := bm.writePackIndexesNew(ctx, buf.Bytes())
	if err != nil {
		return fmt.Errorf("unable to write compacted indexes: %v", err)
	}

	formatLog.Debugf("wrote compacted index (%v bytes) in %v", compactedIndexBlock, time.Since(t0))

	for _, indexBlock := range indexBlocks {
		if indexBlock.FileName == compactedIndexBlock {
			continue
		}

		bm.listCache.deleteListCache(ctx)
		if err := bm.st.DeleteBlock(ctx, indexBlock.FileName); err != nil {
			log.Warningf("unable to delete compacted block %q: %v", indexBlock.FileName, err)
		}
	}

	return nil
}

// Flush completes writing any pending packs and writes pack indexes to the underlyign storage.
func (bm *Manager) Flush(ctx context.Context) error {
	bm.lock()
	defer bm.unlock()

	if err := bm.finishPackLocked(ctx); err != nil {
		return fmt.Errorf("error writing pending block: %v", err)
	}

	if err := bm.flushPackIndexesLocked(ctx); err != nil {
		return fmt.Errorf("error flushing indexes: %v", err)
	}

	return nil
}

// RewriteBlock causes reads and re-writes a given block using the most recent format.
func (bm *Manager) RewriteBlock(ctx context.Context, blockID string) error {
	bi, err := bm.getBlockInfo(blockID)
	if err != nil {
		return err
	}

	data, err := bm.getBlockContentsUnlocked(ctx, bi)
	if err != nil {
		return err
	}

	bm.lock()
	defer bm.unlock()
	return bm.addToPackLocked(ctx, blockID, data, bi.Deleted)
}

// WriteBlock saves a given block of data to a pack group with a provided name and returns a blockID
// that's based on the contents of data written.
func (bm *Manager) WriteBlock(ctx context.Context, data []byte, prefix string) (string, error) {
	if err := validatePrefix(prefix); err != nil {
		return "", err
	}
	blockID := prefix + hex.EncodeToString(bm.hashData(data))

	// block already tracked
	if bi, err := bm.getBlockInfo(blockID); err == nil {
		if !bi.Deleted {
			return blockID, nil
		}
	}

	log.Debugf("WriteBlock(%q) - new", blockID)
	bm.lock()
	defer bm.unlock()
	err := bm.addToPackLocked(ctx, blockID, data, false)
	return blockID, err
}

func validatePrefix(prefix string) error {
	switch len(prefix) {
	case 0:
		return nil
	case 1:
		if prefix[0] >= 'g' && prefix[0] <= 'z' {
			return nil
		}
	}

	return fmt.Errorf("invalid prefix, must be a empty or single letter between 'g' and 'z'")
}

func (bm *Manager) writePackDataNotLocked(ctx context.Context, data []byte) (string, error) {
	blockID := make([]byte, 16)
	if _, err := cryptorand.Read(blockID); err != nil {
		return "", fmt.Errorf("unable to read crypto bytes: %v", err)
	}

	physicalBlockID := fmt.Sprintf("%v%x", PackBlockPrefix, blockID)

	atomic.AddInt32(&bm.stats.WrittenBlocks, 1)
	atomic.AddInt64(&bm.stats.WrittenBytes, int64(len(data)))
	bm.listCache.deleteListCache(ctx)
	if err := bm.st.PutBlock(ctx, physicalBlockID, data); err != nil {
		return "", err
	}

	return physicalBlockID, nil
}

func (bm *Manager) encryptAndWriteBlockNotLocked(ctx context.Context, data []byte, prefix string) (string, error) {
	hash := bm.hashData(data)
	physicalBlockID := prefix + hex.EncodeToString(hash)

	// Encrypt the block in-place.
	atomic.AddInt64(&bm.stats.EncryptedBytes, int64(len(data)))
	data2, err := bm.formatter.Encrypt(data, hash)
	if err != nil {
		return "", err
	}

	atomic.AddInt32(&bm.stats.WrittenBlocks, 1)
	atomic.AddInt64(&bm.stats.WrittenBytes, int64(len(data)))
	bm.listCache.deleteListCache(ctx)
	if err := bm.st.PutBlock(ctx, physicalBlockID, data2); err != nil {
		return "", err
	}

	return physicalBlockID, nil
}

func (bm *Manager) hashData(data []byte) []byte {
	// Hash the block and compute encryption key.
	blockID := bm.formatter.ComputeBlockID(data)
	atomic.AddInt32(&bm.stats.HashedBlocks, 1)
	atomic.AddInt64(&bm.stats.HashedBytes, int64(len(data)))
	return blockID
}

func cloneBytes(b []byte) []byte {
	return append([]byte{}, b...)
}

// GetBlock gets the contents of a given block. If the block is not found returns blob.ErrBlockNotFound.
func (bm *Manager) GetBlock(ctx context.Context, blockID string) ([]byte, error) {
	bi, err := bm.getBlockInfo(blockID)
	if err != nil {
		return nil, err
	}

	if bi.Deleted {
		return nil, storage.ErrBlockNotFound
	}

	return bm.getBlockContentsUnlocked(ctx, bi)
}

func (bm *Manager) getBlockInfo(blockID string) (Info, error) {
	bm.lock()
	defer bm.unlock()

	// check added blocks, not written to any packs.
	if bi, ok := bm.currentPackItems[blockID]; ok {
		return bi, nil
	}

	// added blocks, written to packs but not yet added to indexes
	if bi, ok := bm.packIndexBuilder[blockID]; ok {
		return *bi, nil
	}

	// read from committed block index
	return bm.committedBlocks.getBlock(blockID)
}

// GetIndexBlock gets the contents of a given index block. If the block is not found returns blob.ErrBlockNotFound.
func (bm *Manager) GetIndexBlock(ctx context.Context, blockID string) ([]byte, error) {
	bm.lock()
	defer bm.unlock()

	return bm.getPhysicalBlockInternal(ctx, blockID)
}

// BlockInfo returns information about a single block.
func (bm *Manager) BlockInfo(ctx context.Context, blockID string) (Info, error) {
	bi, err := bm.getBlockInfo(blockID)
	if err != nil {
		return Info{}, err
	}

	if err == nil {
		if bi.Deleted {
			log.Debugf("BlockInfo(%q) - deleted", blockID)
		} else {
			log.Debugf("BlockInfo(%q) - exists in %v", blockID, bi.PackFile)
		}
	} else {
		log.Debugf("BlockInfo(%q) - error %v", err)
	}

	return bi, err
}

// FindUnreferencedStorageFiles returns the list of unreferenced storage blocks.
func (bm *Manager) FindUnreferencedStorageFiles(ctx context.Context) ([]storage.BlockMetadata, error) {
	infos, err := bm.ListBlockInfos("", false)
	if err != nil {
		return nil, fmt.Errorf("unable to list index blocks: %v", err)
	}

	usedPackBlocks := findPackBlocksInUse(infos)
	var unused []storage.BlockMetadata
	err = bm.st.ListBlocks(ctx, PackBlockPrefix, func(bi storage.BlockMetadata) error {
		u := usedPackBlocks[bi.BlockID]
		if u > 0 {
			log.Debugf("pack %v, in use by %v blocks", bi.BlockID, u)
			return nil
		}

		unused = append(unused, bi)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("error listing storage blocks: %v", err)
	}

	return unused, nil
}

func findPackBlocksInUse(infos []Info) map[string]int {
	packUsage := map[string]int{}

	for _, bi := range infos {
		packUsage[bi.PackFile]++
	}

	return packUsage
}

func (bm *Manager) getBlockContentsUnlocked(ctx context.Context, bi Info) ([]byte, error) {
	if bi.Payload != nil {
		return cloneBytes(bi.Payload), nil
	}

	payload, err := bm.blockCache.getContentBlock(ctx, bi.BlockID, bi.PackFile, int64(bi.PackOffset), int64(bi.Length))
	if err != nil {
		return nil, err
	}

	atomic.AddInt32(&bm.stats.ReadBlocks, 1)
	atomic.AddInt64(&bm.stats.ReadBytes, int64(len(payload)))

	iv, err := getPackedBlockIV(bi.BlockID)
	if err != nil {
		return nil, err
	}

	payload, err = bm.formatter.Decrypt(payload, iv)
	if err != nil {
		return nil, err
	}

	atomic.AddInt64(&bm.stats.DecryptedBytes, int64(len(payload)))

	// Since the encryption key is a function of data, we must be able to generate exactly the same key
	// after decrypting the content. This serves as a checksum.
	if err := bm.verifyChecksum(payload, iv); err != nil {
		return nil, fmt.Errorf("invalid checksum at %v offset %v length %v: %v", bi.PackFile, bi.PackOffset, len(payload), err)
	}

	return payload, nil
}

func (bm *Manager) getPhysicalBlockInternal(ctx context.Context, blockID string) ([]byte, error) {
	payload, err := bm.blockCache.getContentBlock(ctx, blockID, blockID, 0, -1)
	if err != nil {
		return nil, err
	}

	iv, err := getPhysicalBlockIV(blockID)
	if err != nil {
		return nil, err
	}

	atomic.AddInt32(&bm.stats.ReadBlocks, 1)
	atomic.AddInt64(&bm.stats.ReadBytes, int64(len(payload)))

	payload, err = bm.formatter.Decrypt(payload, iv)
	atomic.AddInt64(&bm.stats.DecryptedBytes, int64(len(payload)))
	if err != nil {
		return nil, err
	}

	// Since the encryption key is a function of data, we must be able to generate exactly the same key
	// after decrypting the content. This serves as a checksum.
	if err := bm.verifyChecksum(payload, iv); err != nil {
		return nil, err
	}

	return payload, nil
}

func getPackedBlockIV(blockID string) ([]byte, error) {
	return hex.DecodeString(blockID[len(blockID)-(aes.BlockSize*2):])
}

func getPhysicalBlockIV(s string) ([]byte, error) {
	if p := strings.Index(s, "-"); p >= 0 {
		s = s[0:p]
	}
	return hex.DecodeString(s[len(s)-(aes.BlockSize*2):])
}

func (bm *Manager) verifyChecksum(data []byte, blockID []byte) error {
	expected := bm.formatter.ComputeBlockID(data)
	if !bytes.HasSuffix(blockID, expected) {
		atomic.AddInt32(&bm.stats.InvalidBlocks, 1)
		return fmt.Errorf("invalid checksum for blob %x, expected %x", blockID, expected)
	}

	atomic.AddInt32(&bm.stats.ValidBlocks, 1)
	return nil
}

func (bm *Manager) lock() {
	bm.mu.Lock()
	bm.locked = true
}

func (bm *Manager) unlock() {
	if bm.checkInvariantsOnUnlock {
		bm.verifyInvariantsLocked()
	}

	bm.locked = false
	bm.mu.Unlock()
}

func (bm *Manager) assertLocked() {
	if !bm.locked {
		panic("must be locked")
	}
}

// Refresh reloads the committed block indexes.
func (bm *Manager) Refresh(ctx context.Context) (bool, error) {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	log.Debugf("Refresh started")
	t0 := time.Now()
	_, updated, err := bm.loadPackIndexesUnlocked(ctx)
	log.Debugf("Refresh completed in %v and updated=%v", time.Since(t0), updated)
	return updated, err
}

type cachedList struct {
	Timestamp time.Time   `json:"timestamp"`
	Blocks    []IndexInfo `json:"blocks"`
}

// listIndexBlocksFromStorage returns the list of index blocks in the given storage.
// The list of blocks is not guaranteed to be sorted.
func listIndexBlocksFromStorage(ctx context.Context, st storage.Storage) ([]IndexInfo, error) {
	snapshot, err := storage.ListAllBlocksConsistent(ctx, st, newIndexBlockPrefix, math.MaxInt32)
	if err != nil {
		return nil, err
	}

	var results []IndexInfo
	for _, it := range snapshot {
		ii := IndexInfo{
			FileName:  it.BlockID,
			Timestamp: it.Timestamp,
			Length:    it.Length,
		}
		results = append(results, ii)
	}

	return results, err
}

// NewManager creates new block manager with given packing options and a formatter.
func NewManager(ctx context.Context, st storage.Storage, f FormattingOptions, caching CachingOptions) (*Manager, error) {
	return newManagerWithOptions(ctx, st, f, caching, time.Now)
}

func newManagerWithOptions(ctx context.Context, st storage.Storage, f FormattingOptions, caching CachingOptions, timeNow func() time.Time) (*Manager, error) {
	if f.Version < minSupportedReadVersion || f.Version > currentWriteVersion {
		return nil, fmt.Errorf("can't handle repositories created using version %v (min supported %v, max supported %v)", f.Version, minSupportedReadVersion, maxSupportedReadVersion)
	}

	formatter, err := createFormatter(f)
	if err != nil {
		return nil, fmt.Errorf("unable to create block formatter: %v", err)
	}

	blockCache, err := newBlockCache(ctx, st, caching)
	if err != nil {
		return nil, fmt.Errorf("unable to initialize block cache: %v", err)
	}

	listCache, err := newListCache(ctx, st, caching)
	if err != nil {
		return nil, fmt.Errorf("unable to initialize list cache: %v", err)
	}

	blockIndex, err := newCommittedBlockIndex(caching)
	if err != nil {
		return nil, fmt.Errorf("unable to initialize committed block index: %v", err)
	}

	m := &Manager{
		Format:                f,
		timeNow:               timeNow,
		flushPackIndexesAfter: timeNow().Add(flushPackIndexTimeout),
		maxPackSize:           f.MaxPackSize,
		formatter:             formatter,
		currentPackItems:      make(map[string]Info),
		packIndexBuilder:      packindex.NewBuilder(),
		committedBlocks:       blockIndex,
		minPreambleLength:     defaultMinPreambleLength,
		maxPreambleLength:     defaultMaxPreambleLength,
		paddingUnit:           defaultPaddingUnit,
		blockCache:            blockCache,
		listCache:             listCache,
		st:                    st,

		writeFormatVersion:      int32(f.Version),
		closed:                  make(chan struct{}),
		checkInvariantsOnUnlock: os.Getenv("KOPIA_VERIFY_INVARIANTS") != "",
	}

	m.startPackIndexLocked()

	if err := m.CompactIndexes(ctx, autoCompactionMinBlockCount, autoCompactionMaxBlockCount); err != nil {
		return nil, fmt.Errorf("error initializing block manager: %v", err)
	}

	return m, nil
}

func createFormatter(f FormattingOptions) (Formatter, error) {
	sf := FormatterFactories[f.BlockFormat]
	if sf == nil {
		return nil, fmt.Errorf("unsupported block format: %v", f.BlockFormat)
	}

	return sf(f)
}
