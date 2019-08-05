package tensorflow

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"image"
	"time"

	// "image"
	"io/ioutil"
	"os"
	"strconv"
	"strings"

	tf "github.com/tensorflow/tensorflow/tensorflow/go"
	"github.com/tensorflow/tensorflow/tensorflow/go/op"
	"go.uber.org/zap"
	"gocv.io/x/gocv"

	"github.com/snowzach/doods/detector/dconfig"
	"github.com/snowzach/doods/odrpc"
)

type detector struct {
	config odrpc.Detector
	logger *zap.SugaredLogger

	labels map[int]string
	graph  *tf.Graph
	pool   chan *tf.Session
}

func New(c *dconfig.DetectorConfig) (*detector, error) {

	d := &detector{
		labels: make(map[int]string),
		logger: zap.S().With("package", "detector.tensorflow"),
		pool:   make(chan *tf.Session, c.NumConcurrent),
	}

	d.config.Name = c.Name
	d.config.Type = c.Type
	d.config.Model = c.ModelFile
	d.config.Labels = make([]string, 0)
	d.config.Width = -1
	d.config.Height = -1

	// Load labels
	f, err := os.Open(c.LabelFile)
	if err != nil {
		return nil, fmt.Errorf("could not load label", "error", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for x := 1; scanner.Scan(); x++ {
		fields := strings.SplitAfterN(scanner.Text(), " ", 2)
		if len(fields) == 1 {
			d.labels[x] = fields[0]
			d.config.Labels = append(d.config.Labels, fields[0])
		} else if len(fields) == 2 {
			if y, err := strconv.Atoi(strings.TrimSpace(fields[0])); err == nil {
				d.labels[y] = strings.TrimSpace(fields[1])
				d.config.Labels = append(d.config.Labels, strings.TrimSpace(fields[1]))
			}
		}
	}

	// Raw model data
	modelData, err := ioutil.ReadFile(c.ModelFile)
	if err != nil {
		return nil, fmt.Errorf("Could not read model file %s: %v", c.ModelFile, err)
	}

	d.graph = tf.NewGraph()
	if err := d.graph.Import(modelData, ""); err != nil {
		return nil, fmt.Errorf("Could not import model: %v", err)
	}

	// Create sessions
	for x := 0; x < c.NumConcurrent; x++ {
		s, err := tf.NewSession(d.graph, nil)
		if err != nil {
			return nil, fmt.Errorf("Could not create session: %v", err)
		}
		d.pool <- s
	}

	return d, nil

}

func (d *detector) Config() *odrpc.Detector {
	return &d.config
}

func (d *detector) Shutdown() {
	close(d.pool)
	for {
		sess := <-d.pool
		if sess == nil {
			break
		}
		sess.Close()
	}
}

func (d *detector) Detect(ctx context.Context, request *odrpc.DetectRequest) *odrpc.DetectResponse {

	sess := <-d.pool
	defer func() {
		d.pool <- sess
	}()

	// Determine the image type
	imgConfig, imgType, err := image.DecodeConfig(bytes.NewReader(request.Data))
	if err != nil {
		return &odrpc.DetectResponse{
			Id:    request.Id,
			Error: fmt.Sprintf("Could not decode image: %v", err),
		}
	}

	// If the image is not a supported type, convert it to bmp
	if imgType != "png" && imgType != "gif" && imgType != "jpg" && imgType != "bmp" {
		// Convert the image to bmp
		img, err := gocv.IMDecode(request.Data, gocv.IMReadColor)
		if err != nil {
			return &odrpc.DetectResponse{
				Id:    request.Id,
				Error: fmt.Sprintf("Could not decode image: %v", err),
			}
		}
		defer img.Close()

		imgConfig.Width = img.Cols()
		imgConfig.Height = img.Rows()

		// Encode as raw BMP
		request.Data, err = gocv.IMEncode(".bmp", img)
		if err != nil {
			return &odrpc.DetectResponse{
				Id:    request.Id,
				Error: fmt.Sprintf("error encoding bmp %v", err),
			}
		}
		imgType = "bmp"
	}

	scope := op.NewScope()
	imgInput := op.Placeholder(scope, tf.String)

	var decodeOutput tf.Output
	switch imgType {
	case "gif":
		decodeOutput = op.DecodeGif(scope, imgInput)
	case "jpg":
		decodeOutput = op.DecodeJpeg(scope, imgInput, op.DecodeJpegChannels(3))
	case "png":
		decodeOutput = op.DecodePng(scope, imgInput, op.DecodePngChannels(3))
	case "bmp":
		decodeOutput = op.DecodeBmp(scope, imgInput, op.DecodeBmpChannels(3))
	}

	imgOutput := op.ExpandDims(scope, decodeOutput, op.Const(scope.SubScope("make_batch"), int32(0)))
	graph, err := scope.Finalize()

	imgTensor, err := tf.NewTensor(string(request.Data)) // FIX: Convert back to string
	if err != nil {
		return &odrpc.DetectResponse{
			Id:    request.Id,
			Error: fmt.Sprintf("could not create input tensor %v", err),
		}
	}

	// Execute that graph to decode this one image
	imgSess, err := tf.NewSession(graph, nil)
	if err != nil {
		return &odrpc.DetectResponse{
			Id:    request.Id,
			Error: fmt.Sprintf("could not create image session %v", err),
		}
	}

	// Run the detection
	decodedImgTensor, err := imgSess.Run(map[tf.Output]*tf.Tensor{imgInput: imgTensor}, []tf.Output{imgOutput}, nil)
	if err != nil {
		return &odrpc.DetectResponse{
			Id:    request.Id,
			Error: fmt.Sprintf("error converting image %v", err),
		}
	}

	// Get all the input and output operations
	inputop := d.graph.Operation("image_tensor")
	// Output ops
	o1 := d.graph.Operation("detection_boxes")
	o2 := d.graph.Operation("detection_scores")
	o3 := d.graph.Operation("detection_classes")
	o4 := d.graph.Operation("num_detections")

	start := time.Now()

	output, err := sess.Run(
		map[tf.Output]*tf.Tensor{
			inputop.Output(0): decodedImgTensor[0],
		},
		[]tf.Output{
			o1.Output(0),
			o2.Output(0),
			o3.Output(0),
			o4.Output(0),
		},
		nil)
	if err != nil {
		return &odrpc.DetectResponse{
			Id:    request.Id,
			Error: fmt.Sprintf("error running detection %v", err),
		}
	}

	scores := output[1].Value().([][]float32)[0]
	classes := output[2].Value().([][]float32)[0]
	locations := output[0].Value().([][][]float32)[0]
	count := int(output[3].Value().([]float32)[0])

	d.logger.Debugw("Detection", "scores", scores, "classes", classes, "locations", locations, "count", count)

	detections := make([]*odrpc.Detection, 0)
	for i := 0; i < count; i++ {
		// Get the label
		label, ok := d.labels[int(classes[i])]
		if !ok {
			d.logger.Warnw("Missing label", "index", classes[i])
		}

		// We have this class listed explicitly
		if score, ok := request.Detect[label]; ok {
			// Does it meet the score?
			if scores[i]*100.0 < score {
				continue
			}
			// We have a wildcard score
		} else if score, ok := request.Detect["*"]; ok {
			if scores[i]*100.0 < score {
				continue
			}
		} else if len(request.Detect) != 0 {
			// It's not listed
			continue
		}

		detection := &odrpc.Detection{
			Y1:         int32(locations[i][0] * float32(imgConfig.Height)),
			X1:         int32(locations[i][1] * float32(imgConfig.Width)),
			Y2:         int32(locations[i][2] * float32(imgConfig.Height)),
			X2:         int32(locations[i][3] * float32(imgConfig.Width)),
			Label:      label,
			Confidence: scores[i] * 100.0,
		}
		// Cleanup the bounds
		if detection.Y1 < 0 {
			detection.Y1 = 0
		}
		if detection.X1 < 0 {
			detection.X1 = 0
		}
		if detection.Y2 > int32(imgConfig.Height) {
			detection.Y2 = int32(imgConfig.Height)
		}
		if detection.X2 > int32(imgConfig.Width) {
			detection.X2 = int32(imgConfig.Width)
		}
		detections = append(detections, detection)

		d.logger.Debugw("Detection", "id", request.Id, "label", detection.Label, "confidence", detection.Confidence, "location", fmt.Sprintf("%d,%d,%d,%d", detection.X1, detection.Y1, detection.X2, detection.Y2))
	}

	d.logger.Infow("Detection Complete", "id", request.Id, "duration", time.Since(start), "detections", len(detections))

	return &odrpc.DetectResponse{
		Id:         request.Id,
		Detections: detections,
	}
}
