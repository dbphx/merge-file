package merge

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"

	"github.com/ml/merge-pdf/backend/internal/model"
)

const mergeBatchSize = 3

var supportedUploadExtensions = map[string]struct{}{
	".pdf": {},
	".png": {},
	".jpg": {},
	".jpeg": {},
}

// SortInputs keeps merge order deterministic when users or Drive provide duplicate positions.
func SortInputs(inputs []model.MergeFileInput) {
	sort.Slice(inputs, func(i, j int) bool {
		if inputs[i].Order == inputs[j].Order {
			return inputs[i].Name < inputs[j].Name
		}
		return inputs[i].Order < inputs[j].Order
	})
}

// MergeFiles delegates PDF assembly to pdfcpu so the backend stays in-process and script-free.
func MergeFiles(workDir, outputName string, inputs []model.MergeFileInput) (string, error) {
	if len(inputs) == 0 {
		return "", fmt.Errorf("no files to merge")
	}

	SortInputs(inputs)

	paths := make([]string, 0, len(inputs))
	for _, input := range inputs {
		paths = append(paths, input.LocalPath)
	}

	outputPath := filepath.Join(workDir, outputName)
	if err := mergeInBatches(workDir, paths, outputPath); err != nil {
		return "", fmt.Errorf("merge pdf files: %w", err)
	}

	if _, err := os.Stat(outputPath); err != nil {
		return "", fmt.Errorf("merged output missing: %w", err)
	}

	return outputPath, nil
}

func mergeInBatches(workDir string, paths []string, outputPath string) error {
	if len(paths) <= mergeBatchSize {
		return pdfapi.MergeCreateFile(paths, outputPath, false, nil)
	}

	current := append([]string(nil), paths...)
	round := 0
	for len(current) > mergeBatchSize {
		round += 1
		next := make([]string, 0, (len(current)+mergeBatchSize-1)/mergeBatchSize)
		for batchIndex, start := 0, 0; start < len(current); batchIndex, start = batchIndex+1, start+mergeBatchSize {
			end := start + mergeBatchSize
			if end > len(current) {
				end = len(current)
			}

			partPath := filepath.Join(workDir, fmt.Sprintf("merge-part-r%d-b%d.pdf", round, batchIndex))
			if err := pdfapi.MergeCreateFile(current[start:end], partPath, false, nil); err != nil {
				return err
			}
			next = append(next, partPath)
		}
		current = next
	}

	return pdfapi.MergeCreateFile(current, outputPath, false, nil)
}

func SupportsUploadFile(name string) bool {
	_, ok := supportedUploadExtensions[strings.ToLower(filepath.Ext(name))]
	return ok
}

func NormalizeUploadInput(workDir, localPath string) (string, error) {
	switch strings.ToLower(filepath.Ext(localPath)) {
	case ".pdf":
		return localPath, nil
	case ".png":
		outputPath := filepath.Join(workDir, strings.TrimSuffix(filepath.Base(localPath), filepath.Ext(localPath))+"-image.pdf")
		if err := pdfapi.ImportImagesFile([]string{localPath}, outputPath, nil, nil); err != nil {
			return "", fmt.Errorf("convert png to pdf: %w", err)
		}
		return outputPath, nil
	case ".jpg", ".jpeg":
		outputPath := filepath.Join(workDir, strings.TrimSuffix(filepath.Base(localPath), filepath.Ext(localPath))+"-image.pdf")
		if err := pdfapi.ImportImagesFile([]string{localPath}, outputPath, nil, nil); err != nil {
			return "", fmt.Errorf("convert jpg to pdf: %w", err)
		}
		return outputPath, nil
	default:
		return "", fmt.Errorf("unsupported upload file type: %s", filepath.Ext(localPath))
	}
}
