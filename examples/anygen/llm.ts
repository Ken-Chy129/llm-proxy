// Reference actions, not a standalone deployable bundle.
// Put in the platform-generated App's api/lambda/llm.ts; retain its scaffold.
import { Api, Data, Get, Post, useAnygen } from '@anygen/server';
import OpenAI from 'openai';
import { z } from 'zod';

const messageSchema = z.object({
  role: z.string().min(1),
}).passthrough();

const chatSchema = z.object({
  model: z.string().min(1),
  // Preserve content parts, tool calls/results, and extension fields.
  // Message count is not a substitute for a model's context token budget.
  messages: z.array(messageSchema).min(1),
  stream: z.literal(false).optional(),
}).passthrough();

export const chatCompletions = Api(
  Post('/v1/chat/completions'),
  Data(chatSchema),
  async ({ data }) => {
    const { baseUrl, apiKey } = useAnygen().llmEndpoint();
    const client = new OpenAI({ baseURL: baseUrl, apiKey, maxRetries: 0 });
    // Never log/return/persist the internal endpoint credential.
    return client.chat.completions.create({
      ...data,
      stream: false,
    } as OpenAI.Chat.Completions.ChatCompletionCreateParamsNonStreaming);
  },
);

export const models = Api(
  Get('/v1/models'),
  async () => ({ object: 'list', data: await useAnygen().listModels() }),
);
