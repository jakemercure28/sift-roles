import type { Job, Processor } from 'bullmq';

export const JOB_ANALYZE_KEYWORDS = 'analyzeKeywords';

export interface AnalyzeKeywordsPayload {
  readonly keywords: readonly string[];
  readonly trace_id?: string;
}

export interface AnalyzeKeywordsResult {
  readonly keywordCount: number;
}

export type QueueJobName = typeof JOB_ANALYZE_KEYWORDS;
export type QueueJobPayload = AnalyzeKeywordsPayload;
export type QueueJobResult = AnalyzeKeywordsResult;

export interface QueueHandlers {
  readonly analyzeKeywords: (
    payload: AnalyzeKeywordsPayload
  ) => Promise<AnalyzeKeywordsResult>;
}

export const defaultHandlers: QueueHandlers = {
  async analyzeKeywords(payload) {
    return { keywordCount: payload.keywords.length };
  },
};

export function createProcessor(
  handlers: QueueHandlers = defaultHandlers
): Processor<QueueJobPayload, QueueJobResult, QueueJobName> {
  return async (
    job: Job<QueueJobPayload, QueueJobResult, QueueJobName>
  ): Promise<QueueJobResult> => {
    switch (job.name) {
      case JOB_ANALYZE_KEYWORDS:
        return handlers.analyzeKeywords(job.data);
      default:
        throw new Error(`unknown BullMQ job: ${job.name}`);
    }
  };
}
