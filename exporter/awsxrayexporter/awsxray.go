// Copyright 2019, OpenTelemetry Authors
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

package awsxrayexporter // import "github.com/open-telemetry/opentelemetry-collector-contrib/exporter/awsxrayexporter"

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/xray"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/exporterhelper"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/awsxrayexporter/internal/translator"
	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/aws/awsutil"
	awsxray "github.com/open-telemetry/opentelemetry-collector-contrib/internal/aws/xray"
	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/aws/xray/telemetry"
)

const (
	maxSegmentsPerPut = int(50) // limit imposed by PutTraceSegments API
)

// newTracesExporter creates an exporter.Traces that converts to an X-Ray PutTraceSegments
// request and then posts the request to the configured region's X-Ray endpoint.
func newTracesExporter(
	cfg *Config,
	set exporter.CreateSettings,
	cn awsutil.ConnAttr,
	registry telemetry.Registry,
) (exporter.Traces, error) {
	typeLog := zap.String("type", string(set.ID.Type()))
	nameLog := zap.String("name", set.ID.String())
	logger := set.Logger
	awsConfig, session, err := awsutil.GetAWSConfigSession(logger, cn, &cfg.AWSSessionSettings)
	if err != nil {
		return nil, err
	}
	xrayClient := awsxray.NewXRayClient(logger, awsConfig, set.BuildInfo, session)
	sender := telemetry.NewNopSender()
	if cfg.TelemetryConfig.Enabled {
		opts := telemetry.ToOptions(cfg.TelemetryConfig, session, &cfg.AWSSessionSettings)
		opts = append(opts, telemetry.WithLogger(set.Logger))
		sender = registry.Register(set.ID, cfg.TelemetryConfig, xrayClient, opts...)
	}
	return exporterhelper.NewTracesExporter(
		context.TODO(),
		set,
		cfg,
		func(ctx context.Context, td ptrace.Traces) error {
			var err error
			logger.Debug("TracesExporter", typeLog, nameLog, zap.Int("#spans", td.SpanCount()))

			documents := extractResourceSpans(cfg, logger, td)

			for offset := 0; offset < len(documents); offset += maxSegmentsPerPut {
				var nextOffset int
				if offset+maxSegmentsPerPut > len(documents) {
					nextOffset = len(documents)
				} else {
					nextOffset = offset + maxSegmentsPerPut
				}
				input := xray.PutTraceSegmentsInput{TraceSegmentDocuments: documents[offset:nextOffset]}
				logger.Debug("request: " + input.String())
				output, localErr := xrayClient.PutTraceSegments(&input)
				if localErr != nil {
					logger.Debug("response error", zap.Error(localErr))
					err = wrapErrorIfBadRequest(localErr) // record error
					sender.RecordConnectionError(localErr)
				} else {
					sender.RecordSegmentsSent(len(input.TraceSegmentDocuments))
				}
				if output != nil {
					logger.Debug("response: " + output.String())
				}
				if err != nil {
					break
				}
			}
			return err
		},
		exporterhelper.WithStart(func(context.Context, component.Host) error {
			sender.Start()
			return nil
		}),
		exporterhelper.WithShutdown(func(context.Context) error {
			sender.Stop()
			_ = logger.Sync()
			return nil
		}),
	)
}

func extractResourceSpans(config component.Config, logger *zap.Logger, td ptrace.Traces) []*string {
	documents := make([]*string, 0, td.SpanCount())

	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rspans := td.ResourceSpans().At(i)
		resource := rspans.Resource()
		for j := 0; j < rspans.ScopeSpans().Len(); j++ {
			spans := rspans.ScopeSpans().At(j).Spans()
			for k := 0; k < spans.Len(); k++ {
				document, localErr := translator.MakeSegmentDocumentString(
					spans.At(k), resource,
					config.(*Config).IndexedAttributes,
					config.(*Config).IndexAllAttributes,
					config.(*Config).LogGroupNames)
				if localErr != nil {
					logger.Debug("Error translating span.", zap.Error(localErr))
					continue
				}
				documents = append(documents, &document)
			}
		}
	}
	return documents
}

func wrapErrorIfBadRequest(err error) error {
	var rfErr awserr.RequestFailure
	if errors.As(err, &rfErr) && rfErr.StatusCode() < 500 {
		return consumererror.NewPermanent(err)
	}
	return err
}
