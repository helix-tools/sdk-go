package producer

import (
	"maps"
	"regexp"
	"strings"
	"time"

	"github.com/helix-tools/sdk-go/types"
)

var slugRegex = regexp.MustCompile(`[^a-z0-9-]+`)

func slugify(name string) string {
	slug := strings.ToLower(name)
	slug = slugRegex.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return "dataset"
	}
	return slug
}

func (p *Producer) generateDatasetID(name string, explicitID *string) string {
	if explicitID != nil && *explicitID != "" {
		return *explicitID
	}
	timestamp := time.Now().UTC().Format("20060102150405")
	return p.CustomerID + "-" + slugify(name) + "-" + timestamp
}

func defaultPricing() map[string]any {
	return map[string]any{
		"basic":        map[string]any{"amount": 0, "currency": "USD", "interval": "monthly"},
		"professional": map[string]any{"amount": 0, "currency": "USD", "interval": "monthly"},
		"enterprise":   map[string]any{"amount": 0, "currency": "USD", "interval": "monthly"},
	}
}

func defaultStats() map[string]any {
	return map[string]any{
		"subscriber_count":     0,
		"download_count":       0,
		"download_count_7d":    0,
		"download_count_30d":   0,
		"view_count":           0,
		"last_downloaded_at":   nil,
		"avg_download_size_mb": 0,
	}
}

func defaultValidation(recordCount int) map[string]any {
	return map[string]any{
		"validated":          false,
		"validation_errors":  []any{},
		"row_count":          recordCount,
		"data_quality_score": 0,
	}
}

func deepMergeMaps(base, overrides map[string]any) map[string]any {
	if overrides == nil {
		return base
	}
	for key, value := range overrides {
		if existing, ok := base[key]; ok {
			existingMap, existingIsMap := existing.(map[string]any)
			valueMap, valueIsMap := value.(map[string]any)
			if existingIsMap && valueIsMap {
				deepMergeMaps(existingMap, valueMap)
				continue
			}
		}
		base[key] = value
	}
	return base
}

func cloneOverrides(overrides map[string]any) map[string]any {
	if overrides == nil {
		return nil
	}
	return maps.Clone(overrides)
}

func (p *Producer) buildDatasetPayload(
	datasetName string,
	description string,
	category string,
	dataFreshness types.DataFreshness,
	s3Key string,
	finalSize int64,
	combinedMetadata map[string]any,
	analysis *AnalysisResult,
	overrides map[string]any,
) map[string]any {
	overrideCopy := cloneOverrides(overrides)

	var explicitID *string
	if overrideCopy != nil {
		if idValue, ok := overrideCopy["_id"].(string); ok && idValue != "" {
			explicitID = &idValue
			delete(overrideCopy, "_id")
		} else if idValue, ok := overrideCopy["id"].(string); ok && idValue != "" {
			explicitID = &idValue
			delete(overrideCopy, "id")
		}
	}

	datasetID := p.generateDatasetID(datasetName, explicitID)

	var metadataPayload map[string]any
	if combinedMetadata != nil {
		metadataPayload = maps.Clone(combinedMetadata)
	} else {
		metadataPayload = map[string]any{}
	}

	if _, exists := metadataPayload["file_format"]; !exists {
		metadataPayload["file_format"] = "json"
	}
	if _, exists := metadataPayload["encoding"]; !exists {
		metadataPayload["encoding"] = "utf-8"
	}

	recordCount := 0
	schema := map[string]any{}
	fieldEmptiness := map[string]float64{}
	if analysis != nil {
		recordCount = analysis.RecordCount
		if analysis.Schema != nil {
			schema = analysis.Schema
		}
		if analysis.FieldEmptiness != nil {
			fieldEmptiness = analysis.FieldEmptiness
		}
		metadataPayload["field_emptiness"] = fieldEmptiness
		metadataPayload["schema"] = schema
		metadataPayload["record_count"] = analysis.RecordCount
		if analysis.AnalysisErrors > 0 {
			metadataPayload["analysis_errors"] = analysis.AnalysisErrors
		}
	} else {
		metadataPayload["field_emptiness"] = map[string]float64{}
		metadataPayload["schema"] = map[string]any{}
		metadataPayload["record_count"] = 0
	}

	now := time.Now().UTC()
	nowISO := now.Format(time.RFC3339)
	version := now.Format("2006-01-02")

	payload := map[string]any{
		"_id":               datasetID,
		"id":                datasetID,
		"name":              datasetName,
		"description":       description,
		"producer_id":       p.CustomerID,
		"category":          category,
		"data_freshness":    string(dataFreshness),
		"visibility":        "private",
		"status":            "active",
		"access_tier":       "free",
		"s3_key":            s3Key,
		"s3_bucket_name":    p.BucketName,
		"s3_bucket":         p.BucketName,
		"size_bytes":        finalSize,
		"record_count":      recordCount,
		"version":           version,
		"version_notes":     "",
		"parent_dataset_id": nil,
		"is_latest_version": true,
		"metadata":          metadataPayload,
		"schema":            schema,
		"validation":        defaultValidation(recordCount),
		"tags":              []string{},
		"pricing":           defaultPricing(),
		"stats":             defaultStats(),
		"last_updated":      nowISO,
		"created_at":        nowISO,
		"created_by":        p.CustomerID,
		"updated_at":        nowISO,
		"updated_by":        p.CustomerID,
		"deleted_at":        nil,
		"deleted_by":        nil,
	}

	return deepMergeMaps(payload, overrideCopy)
}
