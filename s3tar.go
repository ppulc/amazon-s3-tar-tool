package s3tar

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/remeh/sizedwaitgroup"
)

const (
	blockSize    = int64(512)
	beginningPad = 5 * 1024 * 1024
	fileSizeMin  = beginningPad
)

var (
	accum int64 = 0
	pad         = make([]byte, beginningPad)
	rc    *RecursiveConcat
)

func ServerSideTar(incoming context.Context, svc *s3.Client, opts *S3TarS3Options) {

	ctx := context.WithValue(incoming, contextKeyS3Client, svc)
	start := time.Now()

	var objectList []*S3Obj
	if opts.SrcManifest != "" {
		var err error
		objectList, err = LoadCSV(ctx, opts.SrcManifest, opts.SkipManifestHeader)
		if err != nil {
			log.Fatal(err.Error())
		}
	} else if opts.SrcBucket != "" && opts.SrcPrefix != "" {
		objectList = listAllObjects(ctx, svc, opts.SrcBucket, opts.SrcPrefix)
	} else {
		log.Fatal("Error with source data sourcing")
	}

	log.Printf("Processing %d files", len(objectList))

	smallFiles := false

	totalSize := int64(0)
	for _, o := range objectList {
		totalSize += o.Size
		if o.Size < int64(beginningPad) {
			smallFiles = true
		}
	}

	if totalSize < fileSizeMin {
		Fatalf(ctx, "Total size (%d) of all archives is less than 5MB. Include more files", totalSize)
	}

	log.Printf("%s %s %s", opts.DstBucket, opts.DstPrefix, opts.Region)

	concatObj := NewS3Obj()
	if smallFiles {
		var err error
		rc, err = NewRecursiveConcat(ctx, RecursiveConcatOptions{
			Bucket: opts.DstBucket,
			Key:    opts.DstPrefix,
			Region: opts.Region,
		})
		if err != nil {
			log.Fatal(err.Error())
		}
		Debugf(ctx, "Processing manifest")
		manifestObj, _ := buildManifest(ctx, objectList)
		objectList = append([]*S3Obj{manifestObj}, objectList...)
		Debugf(ctx, "prepended manifest: %s Size: %d len.Data: %d", *manifestObj.Key, manifestObj.Size, len(manifestObj.Data))
		concatObj, _ = processSmallFiles(ctx, objectList, opts.DstKey, opts)
	} else {
		concatObj = processLargeFiles(ctx, svc, objectList, opts)
	}

	Debugf(ctx, "deleting all intermediate objects")
	for _, path := range []string{filepath.Join(opts.DstPrefix, "parts"),
		filepath.Join(opts.DstPrefix, "headers")} {
		deleteList := listAllObjects(ctx, svc, opts.DstBucket, path)
		deleteObjectList(ctx, opts, deleteList)
	}

	Debugf(ctx, "Final Object: s3://%s/%s", concatObj.Bucket, *concatObj.Key)
	elapsed := time.Since(start)
	Infof(ctx, "Time elapsed: %s", elapsed)

}

func processLargeFiles(ctx context.Context, svc *s3.Client, objectList []*S3Obj, opts *S3TarS3Options) *S3Obj {
	ctx = context.WithValue(ctx, contextKeyS3Client, svc)
	concater, err := NewRecursiveConcat(ctx, RecursiveConcatOptions{
		Bucket: opts.DstBucket,
		Key:    opts.DstPrefix,
		Region: opts.Region,
	})
	if err != nil {
		log.Fatal(err.Error())
	}
	manifestObj, _ := buildManifest(ctx, objectList)
	firstPart := buildFirstPart(manifestObj.Data)
	firstPart.Bucket = opts.DstBucket
	objectList = append([]*S3Obj{firstPart}, objectList...)

	wg := sizedwaitgroup.New(25)
	resultsChan := make(chan *S3Obj)
	var bytesAccum int64
	for i, obj := range objectList {
		var p1 = obj
		var p2 *S3Obj = nil
		next := i + 1
		if next < len(objectList) {
			h := buildHeader(objectList[next], p1, false)
			p2 = &h
			bytesAccum += p1.Size + p2.Size
		} else {
			lastblockSize := findPadding(bytesAccum + obj.Size)
			if lastblockSize == 0 {
				lastblockSize = blockSize
			}
			lastblockSize += (blockSize * 2)
			lastBytes := make([]byte, lastblockSize)
			endPadding := NewS3Obj()
			endPadding.AddData(lastBytes)
			endPadding.NoHeaderRequired = true
			p2 = endPadding
		}
		pairs := []*S3Obj{p1, p2}
		name := fmt.Sprintf("%d.part-%d.hdr", i, next)
		key := filepath.Join(opts.DstPrefix, name)
		wg.Add()
		go func(pairs []*S3Obj, key string, partNum int) {
			res, err := concater.ConcatObjects(ctx, pairs, opts.DstBucket, key)
			if err != nil {
				Fatalf(ctx, err.Error())
			}
			wg.Done()
			res.PartNum = partNum
			resultsChan <- res
		}(pairs, key, i+1)

	}
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	results := []*S3Obj{}
	for r := range resultsChan {
		results = append(results, r)
	}
	sort.Sort(byPartNum(results))

	tempKey := filepath.Join(opts.DstPrefix, opts.DstKey+".temp")
	concatObj, err := concatObjects(ctx, svc, 0, results, opts.DstBucket, tempKey)
	if err != nil {
		Fatalf(ctx, err.Error())
	}

	finalObject, err := redistribute(ctx, concatObj, beginningPad, opts.DstBucket, opts.DstKey)
	if err != nil {
		Fatalf(ctx, err.Error())
	}

	Infof(ctx, "Finished: s3://%s/%s", finalObject.Bucket, *finalObject.Key)
	return finalObject

}

// redistribute will try to evenly distribute the object into equal size parts.
// it will also trim whatever offset passed, helpful to remove the front padding
func redistribute(ctx context.Context, obj *S3Obj, trimoffset int64, bucket, key string) (*S3Obj, error) {
	finalSize := obj.Size - trimoffset
	min, max, mid := findMinMaxPartRange(finalSize)
	var r int64 = 0
	for i := max; i >= min; i-- {
		r = finalSize % i
		if r == 0 {
			mid = i
			break
		}
	}

	partSize := finalSize / mid
	Warnf(ctx, "parts: %d", mid)
	Warnf(ctx, "rrrrrrrr:\t%d", r)
	Warnf(ctx, "FinalSize:\t%d", finalSize)
	Warnf(ctx, "total:\t%d", partSize*mid)
	Warnf(ctx, "PartSize:\t%d", partSize)
	var start int64 = 0
	type IndexLoc struct {
		Start int64
		End   int64
		Size  int64
	}
	indexList := []IndexLoc{}
	for start = 0; start < finalSize; start = start + partSize {
		i := IndexLoc{
			Start: trimoffset + start,
			End:   trimoffset + start + partSize,
			Size:  partSize,
		}
		indexList = append(indexList, i)
		Debugf(ctx, "%v-%v", i.Start, i.End)
	}
	if indexList[len(indexList)-1].End != obj.Size {
		indexList[len(indexList)-1].End = obj.Size
	}

	complete := NewS3Obj()
	client := GetS3Client(ctx)
	output, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		Fatalf(ctx, err.Error())
	}
	var accumSize int64 = 0
	uploadId := *output.UploadId
	parts := []types.CompletedPart{}
	m := sync.RWMutex{}
	swg := sizedwaitgroup.New(100)
	for i, r := range indexList {
		partNum := int32(i + 1)
		copySourceRange := fmt.Sprintf("bytes=%d-%d", r.Start, r.End-1)
		input := s3.UploadPartCopyInput{
			Bucket:          &bucket,
			Key:             &key,
			PartNumber:      partNum,
			UploadId:        &uploadId,
			CopySource:      aws.String(obj.Bucket + "/" + *obj.Key),
			CopySourceRange: aws.String(copySourceRange),
		}
		swg.Add()
		go func(input s3.UploadPartCopyInput) {
			defer swg.Done()
			Debugf(ctx, "UploadPartCopy (s3://%s/%s) into:\n\ts3://%s/%s", *input.Bucket, *input.Key, bucket, key)
			r, err := client.UploadPartCopy(ctx, &input)
			if err != nil {
				Debugf(ctx, "error for s3://%s/%s", *input.Bucket, *input.Key)
				Debugf(ctx, "CopySourceRange %s", *input.CopySourceRange)
				panic(err)
			}
			m.Lock()
			parts = append(parts, types.CompletedPart{
				ETag:       r.CopyPartResult.ETag,
				PartNumber: input.PartNumber})
			m.Unlock()
		}(input)

	}
	swg.Wait()
	sort.Slice(parts, func(i, j int) bool {
		return parts[i].PartNumber < parts[j].PartNumber
	})

	completeOutput, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   &bucket,
		Key:      &key,
		UploadId: &uploadId,
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: parts,
		},
	})
	if err != nil {
		Fatalf(ctx, err.Error())
	}
	now := time.Now()
	complete = &S3Obj{
		Bucket: *completeOutput.Bucket,
		Object: types.Object{
			Key:          completeOutput.Key,
			ETag:         completeOutput.ETag,
			Size:         accumSize,
			LastModified: &now,
		},
	}
	return complete, nil

}

func processSmallFiles(ctx context.Context, objectList []*S3Obj, dstKey string, opts *S3TarS3Options) (*S3Obj, error) {

	Debugf(ctx, "processSmallFiles path")
	client := GetS3Client(ctx)

	indexList, totalSize := createGroups(objectList)
	lastblockSize := findPadding(totalSize)
	if lastblockSize == 0 {
		lastblockSize = blockSize
	}
	lastblockSize += (blockSize * 2)
	lastBytes := make([]byte, lastblockSize)
	endPadding := NewS3Obj()
	endPadding.AddData(lastBytes)
	endPadding.NoHeaderRequired = true
	objectList = append(objectList, endPadding)
	indexList[len(indexList)-1].End = len(objectList) - 1

	m := sync.Mutex{}
	groups := []*S3Obj{}
	swg := sizedwaitgroup.New(100)
	Debugf(ctx, "Created %d parts", len(indexList))
	for i, p := range indexList {
		Debugf(ctx, "Part %06d range: %d - %d", i+1, p.Start, p.End)
		swg.Add()
		go func(start, end int) {
			defer swg.Done()
			newPart, err := _processSmallFiles(ctx, objectList, start, end, opts)
			if err != nil {
				panic(err)
			}
			m.Lock()
			newPart.PartNum = start
			groups = append(groups, newPart)
			m.Unlock()
		}(p.Start, p.End)
	}

	Debugf(ctx, "Waiting for threads")
	swg.Wait()
	sort.Sort(byPartNum(groups))

	// reset partNum counts.
	// Figure out if the final concat needs to be recursive
	recursiveConcat := false
	for x := 0; x < len(groups)-1; x++ { //ignore last piece
		groups[x].PartNum = x + 1
		// Debugf(ctx,"Group %05d - Size: %d", x, groups[x].Size/1024/1024)
		if groups[x].Size < int64(fileSizeMin) {
			recursiveConcat = true
		}
	}
	groups[len(groups)-1].PartNum = len(groups) // setup the last PartNum since we skipped it

	finalObject := NewS3Obj()
	if recursiveConcat {
		padObject := &S3Obj{
			Object: types.Object{
				Key:  aws.String("pad_file"),
				Size: int64(len(pad)),
			},
			Data: pad}
		for i := 0; i < len(groups); i++ {
			var err error
			var pair []*S3Obj
			if i == 0 {
				pair = []*S3Obj{padObject, groups[i]}
			} else {
				pair = []*S3Obj{finalObject, groups[i]}
			}
			trim := 0
			if i == len(groups)-1 {
				trim = beginningPad
			}
			Debugf(ctx, "Concat(%s,%s)", *pair[0].Key, *pair[1].Key)
			finalObject, err = concatObjects(ctx, client, trim, pair, opts.DstBucket, opts.DstKey)
			if err != nil {
				log.Fatal(err.Error())
			}
		}
	} else {
		var err error
		finalObject, err = concatObjects(ctx, client, 0, groups, opts.DstBucket, opts.DstKey)
		if err != nil {
			Debugf(ctx, "error recursion on final\n%s", err.Error())
			return NewS3Obj(), err
		}
	}
	return finalObject, nil

}

func _processSmallFiles(ctx context.Context, objectList []*S3Obj, start, end int, opts *S3TarS3Options) (*S3Obj, error) {
	parentPartsKey := filepath.Join(opts.DstPrefix, "parts")
	parts := []*S3Obj{}
	for i, partNum := start, 0; i <= end; i, partNum = i+1, partNum+1 {
		Debugf(ctx, "Processing: %s", *objectList[i].Key)
		// some objects my not need a tar header generated (like the last piece)
		if objectList[i].NoHeaderRequired {
			parts = append(parts, objectList[i])
		} else {
			prev := NewS3Obj()
			if (i - 1) >= 0 {
				prev = objectList[i-1]
			}
			header := buildHeader(objectList[i], prev, false)
			header.Bucket = opts.DstBucket
			pairs := []*S3Obj{&header, {
				Object:  objectList[i].Object, // fix this
				Bucket:  opts.SrcBucket,
				Data:    objectList[i].Data,
				PartNum: partNum,
			}}
			parts = append(parts, pairs...)
		}

	}

	batchName := fmt.Sprintf("%d-%d", start, end)
	dstKey := filepath.Join(parentPartsKey, strings.Join([]string{"iteration", "batch", batchName}, "."))
	finalPart, err := rc.ConcatObjects(ctx, parts, opts.DstBucket, dstKey)
	if err != nil {
		Debugf(ctx, "%s", dstKey)
		Debugf(ctx, "error recursion on final\n%s", err.Error())
		return NewS3Obj(), err
	}

	return finalPart, nil
}

func createGroups(objectList []*S3Obj) ([]Index, int64) {

	// Walk through all the parts and build groups of 10MB
	// so we can parallelize.
	indexList := []Index{}
	last := 0

	h := buildHeader(objectList[0], nil, false)
	currSize := h.Size + objectList[0].Size
	var totalSize int64 = currSize
	for i := 1; i < len(objectList); i++ {
		var prev *S3Obj
		if (i - 1) >= 0 {
			prev = objectList[i-1]
		}
		header := buildHeader(objectList[i], prev, false)
		l := int64(len(header.Data)) + objectList[i].Size
		currSize += l
		totalSize += l
		if currSize > int64(1024*1024*10) {
			indexList = append(indexList, Index{Start: last, End: i, Size: int(currSize)})
			last, currSize = i+1, 0
		}
	}

	if len(indexList) == 0 {
		indexList = []Index{
			{
				Start: 0,
				End:   len(objectList) - 1,
				Size:  int(totalSize),
			},
		}
	}

	// Make the last part include everything till the end.
	// We don't want something that is less than 5MB
	indexList[len(indexList)-1].End = len(objectList) - 1
	indexList[len(indexList)-1].Size = indexList[len(indexList)-1].Size + int(currSize)
	return indexList, totalSize
}

func concatObjects(ctx context.Context, client *s3.Client, trimFirstBytes int, objectList []*S3Obj, bucket, key string) (*S3Obj, error) {
	// postfix, err := randomHex(16)
	// if err != nil {
	// 	panic(err)
	// }
	// key = key + "." + postfix
	complete := NewS3Obj()
	output, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return complete, err
	}
	var accumSize int64 = 0
	uploadId := *output.UploadId
	parts := []types.CompletedPart{}
	m := sync.RWMutex{}
	swg := sizedwaitgroup.New(100)
	for i, object := range objectList {
		partNum := int32(i + 1)
		if len(object.Data) > 0 {
			accumSize += int64(len(object.Data))
			input := &s3.UploadPartInput{
				Bucket:     &bucket,
				Key:        &key,
				PartNumber: partNum,
				UploadId:   &uploadId,
				Body:       bytes.NewReader(object.Data),
			}
			swg.Add()
			go func(input *s3.UploadPartInput) {
				defer swg.Done()
				Debugf(ctx, "UploadPart (bytes) into: %s/%s", *input.Bucket, *input.Key)
				r, err := client.UploadPart(ctx, input)
				if err != nil {
					Debugf(ctx, "error for s3://%s/%s", *input.Bucket, *input.Key)
					panic(err)
				}
				m.Lock()
				parts = append(parts, types.CompletedPart{
					ETag:       r.ETag,
					PartNumber: input.PartNumber})
				m.Unlock()
			}(input)
		} else {
			var copySourceRange string
			if i == 0 && trimFirstBytes > 0 {
				copySourceRange = fmt.Sprintf("bytes=%d-%d", trimFirstBytes, object.Size-1)
				accumSize += object.Size - int64(trimFirstBytes)
			} else {
				copySourceRange = fmt.Sprintf("bytes=0-%d", object.Size-1)
				accumSize += object.Size
			}
			input := s3.UploadPartCopyInput{
				Bucket:          &bucket,
				Key:             &key,
				PartNumber:      partNum,
				UploadId:        &uploadId,
				CopySource:      aws.String(object.Bucket + "/" + *object.Key),
				CopySourceRange: aws.String(copySourceRange),
			}
			swg.Add()
			go func(input s3.UploadPartCopyInput) {
				defer swg.Done()
				Debugf(ctx, "UploadPartCopy (s3://%s/%s) into:\n\ts3://%s/%s", *input.Bucket, *input.Key, bucket, key)
				r, err := client.UploadPartCopy(ctx, &input)
				if err != nil {
					Debugf(ctx, "error for s3://%s/%s", *input.Bucket, *input.Key)
					panic(err)
				}
				m.Lock()
				parts = append(parts, types.CompletedPart{
					ETag:       r.CopyPartResult.ETag,
					PartNumber: input.PartNumber})
				m.Unlock()
			}(input)
		}
	}

	swg.Wait()
	sort.Slice(parts, func(i, j int) bool {
		return parts[i].PartNumber < parts[j].PartNumber
	})

	completeOutput, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   &bucket,
		Key:      &key,
		UploadId: &uploadId,
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: parts,
		},
	})
	if err != nil {
		return complete, err
	}
	// fmt.Printf("%+v", completeOutput)
	now := time.Now()
	complete = &S3Obj{
		Bucket: *completeOutput.Bucket,
		Object: types.Object{
			Key:          completeOutput.Key,
			ETag:         completeOutput.ETag,
			Size:         accumSize,
			LastModified: &now,
		},
	}
	return complete, nil
}
