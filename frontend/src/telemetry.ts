import { WebTracerProvider } from '@opentelemetry/sdk-trace-web';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-http';
import { BatchSpanProcessor } from '@opentelemetry/sdk-trace-web';
import { registerInstrumentations } from '@opentelemetry/instrumentation';
import { FetchInstrumentation } from '@opentelemetry/instrumentation-fetch';
import { DocumentLoadInstrumentation } from '@opentelemetry/instrumentation-document-load';
import { ZoneContextManager } from '@opentelemetry/context-zone';
import { resourceFromAttributes } from '@opentelemetry/resources';
import { ATTR_SERVICE_NAME } from '@opentelemetry/semantic-conventions';

declare global {
  interface Window {
    __VARROA_TELEMETRY__?: {
      disabled?: boolean;
      bffUrl?: string;
      tracesEnabled?: boolean;
      samplerRatio?: number;
    };
  }
}

export function initTelemetry() {
  const config = window.__VARROA_TELEMETRY__ || {};
  if (config.disabled || !config.tracesEnabled) return;

  const bffUrl = config.bffUrl || '/api/v1';

  const exporter = new OTLPTraceExporter({
    url: `${bffUrl}/otel/v1/traces`,
  });

  const provider = new WebTracerProvider({
    resource: resourceFromAttributes({ [ATTR_SERVICE_NAME]: 'varroa-frontend' }),
    spanProcessors: [new BatchSpanProcessor(exporter)],
  });

  provider.register({ contextManager: new ZoneContextManager() });

  registerInstrumentations({
    instrumentations: [
      new FetchInstrumentation({ propagateTraceHeaderCorsUrls: [/.+/] }),
      new DocumentLoadInstrumentation(),
    ],
  });
}
