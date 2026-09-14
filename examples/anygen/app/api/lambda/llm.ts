import { Api, Data, Get, Post, useAnygen } from '@anygen/server';
import { z } from 'zod';

const chatSchema = z.object({
  model: z.string().min(1),
  messages: z.array(z.object({ role: z.string().min(1) }).passthrough()).min(1),
  stream: z.literal(false).optional(),
}).passthrough();

export const chatCompletions = Api(
  Post('/v1/chat/completions'),
  Data(chatSchema),
  async ({ data }) => {
    const { baseUrl, apiKey } = useAnygen().llmEndpoint();
    // Native fetch preserves the full protocol without another SDK dependency
    // or automatic model retries. The platform owns the request deadline.
    const response = await fetch(`${baseUrl.replace(/\/$/, '')}/chat/completions`, {
      method: 'POST',
      redirect: 'error',
      headers: { Authorization: `Bearer ${apiKey}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ ...data, stream: false }),
    });
    if (!response.ok) {
      // Upstream error bodies may echo credentials or internal endpoint details.
      const error = Object.assign(new Error(`LLM upstream returned HTTP ${response.status}`), {
        status: response.status,
      });
      throw error;
    }
    const result: unknown = await response.json();
    return z.object({
      object: z.literal('chat.completion'),
      choices: z.array(z.unknown()),
    }).passthrough().parse(result);
  },
);

export const models = Api(
  Get('/v1/models'),
  async () => ({ object: 'list', data: await useAnygen().listModels() }),
);
