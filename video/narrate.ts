import { execFile } from "node:child_process";
import { mkdir, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";

const run = promisify(execFile);

// Voice-over for the walkthroughs.
//
//   gemini (default when OPENROUTER_API_KEY is set) — Gemini 3.1 Flash TTS via
//     OpenRouter's dedicated `/api/v1/audio/speech` endpoint. A real TTS model:
//     it reads its input and nothing else.
//   gptaudio — `openai/gpt-audio` through chat completions. Kept because it is
//     the only other speech model on OpenRouter, but see LEARNINGS.md: it is a
//     chat model wearing a TTS hat. It paraphrases unless heavily prompted, and
//     it pads: one 25-word line came back as 13 minutes of audio with a
//     word-perfect transcript.
//   say — the macOS built-in. Free, offline, audibly synthetic. No-key fallback.
//
// Narration is re-synthesised on *every* render (a one-word script edit reflows
// every chapter offset after it), so no backend can be slow or costly per line.
export type TtsBackend = "gemini" | "gptaudio" | "say";

// Every one of these reads process.env *when called*, never at module scope.
// The entry point loads .env after its imports have already been evaluated —
// a module-level `const BACKEND = ...` silently resolved to "say" on every
// run, and the only symptom was a robot voice in the finished video.
export function backend(): TtsBackend {
  return (
    (process.env.VIDEO_TTS as TtsBackend | undefined) ??
    (process.env.OPENROUTER_API_KEY ? "gemini" : "say")
  );
}

// Gemini's prebuilt voices, each with a documented character. Charon is the
// "informative" one, which is what a product walkthrough wants; Sulafat (warm),
// Algieba (smooth) and Kore (firm) are the other plausible narrators. All four
// read at a steady 140-150 wpm.
const geminiVoice = () => process.env.VIDEO_VOICE ?? "Charon";
const geminiModel = () =>
  process.env.VIDEO_TTS_MODEL ?? "google/gemini-3.1-flash-tts-preview";
/** Optional natural-language style nudge; Gemini TTS takes direction. */
const geminiStyle = () =>
  process.env.VIDEO_TTS_STYLE ?? "Calm, warm, unhurried product-demo narration.";

const gptAudioVoice = () => process.env.VIDEO_VOICE_OR ?? "ash";

// macOS fallback. `say -v '?'` lists what's installed; out of the box that's
// only the *compact* voices, of which Samantha is the least robotic.
const sayVoice = () => process.env.VIDEO_VOICE_SAY ?? "Samantha";
const sayRate = () => Number(process.env.VIDEO_RATE ?? 165);

// Only gpt-audio needs this. A dedicated TTS model reads its input; a chat
// model answers it ("Understood. Let's get started on the walkthrough…").
const VERBATIM_SYSTEM =
  "You are a speech synthesizer, not an assistant. The user message is a " +
  "script. Speak it back word for word in a calm, warm, unhurried " +
  "product-demo voice. Never acknowledge, never reply, never add or remove a " +
  "single word.";

export type NarrationClip = {
  /** Absolute path to the synthesised audio. */
  file: string;
  /** Exact length in seconds, measured off the rendered file. */
  seconds: number;
};

/** Length of an existing audio/video file, in seconds. */
export async function probeDuration(file: string): Promise<number> {
  const { stdout } = await run("ffprobe", [
    "-v", "error",
    "-show_entries", "format=duration",
    "-of", "csv=p=0",
    file,
  ]);
  const seconds = Number(stdout.trim());
  if (!Number.isFinite(seconds)) {
    throw new Error(`Could not read a duration from ${file}`);
  }
  return seconds;
}

/** Loose enough to ignore curly quotes and em-dashes, strict about words. */
function normalise(text: string): string {
  return text.toLowerCase().replace(/[^a-z0-9]+/g, " ").trim();
}

/**
 * These models emit headerless 24 kHz mono PCM. Wrap it, and trim the dead air
 * they sometimes pad with at either end so the scene timing tracks the words
 * rather than the silence around them.
 */
async function pcmToWav(pcm: Buffer, raw: string, wav: string) {
  await writeFile(raw, pcm);
  await run("ffmpeg", [
    "-y", "-v", "error",
    "-f", "s16le", "-ar", "24000", "-ac", "1",
    "-i", raw,
    "-af",
    "silenceremove=start_periods=1:start_silence=0.15:start_threshold=-45dB:" +
      "stop_periods=-1:stop_duration=0.6:stop_threshold=-45dB",
    wav,
  ]);
  await rm(raw, { force: true });
}

/**
 * Roughly 150 wpm is the pace these read at. Anything past ~2.5x that isn't a
 * slower reading, it's padding or a loop — see LEARNINGS.md.
 */
function durationCeiling(text: string) {
  const words = text.trim().split(/\s+/).length;
  const expected = (words / 150) * 60;
  return { words, expected, ceiling: Math.max(expected * 2.5, expected + 6) };
}

/**
 * Every line is synthesised up front, before a single frame is recorded — so a
 * dropped connection on line nine throws away the eight generations already
 * paid for and the render never starts. Two consecutive `ECONNRESET`s from the
 * speech endpoint cost two runs before this existed.
 *
 * Only *transport* failures are retried: a network error, a 429, or a 5xx. A
 * 4xx is a request this code got wrong and repeating it just spends money
 * slower, and the duration ceiling below stays a hard error because a padded
 * generation is a model pathology worth seeing rather than papering over.
 */
async function fetchSpeech(body: string): Promise<Response> {
  const backoffMs = [1_000, 3_000, 8_000];
  for (let attempt = 0; ; attempt++) {
    const last = attempt >= backoffMs.length;
    let res: Response;
    try {
      res = await fetch("https://openrouter.ai/api/v1/audio/speech", {
        method: "POST",
        headers: {
          Authorization: `Bearer ${process.env.OPENROUTER_API_KEY}`,
          "Content-Type": "application/json",
        },
        body,
      });
    } catch (err) {
      if (last) throw err;
      const why = err instanceof Error ? err.message : String(err);
      console.warn(`    · speech request failed (${why}) — retrying`);
      await new Promise((r) => setTimeout(r, backoffMs[attempt]));
      continue;
    }
    if (res.ok) return res;
    const retriable = res.status === 429 || res.status >= 500;
    if (!retriable || last) {
      throw new Error(
        `Gemini TTS ${res.status}: ${(await res.text()).slice(0, 300)}`,
      );
    }
    console.warn(`    · speech request returned ${res.status} — retrying`);
    await new Promise((r) => setTimeout(r, backoffMs[attempt]));
  }
}

async function synthesiseGemini(
  text: string,
  outDir: string,
  name: string,
): Promise<NarrationClip> {
  const res = await fetchSpeech(
    JSON.stringify({
      model: geminiModel(),
      input: text,
      voice: geminiVoice(),
      // Gemini TTS rejects every other format, mp3 included.
      response_format: "pcm",
      instructions: geminiStyle(),
    }),
  );

  const file = path.join(outDir, `${name}.wav`);
  await pcmToWav(
    Buffer.from(await res.arrayBuffer()),
    path.join(outDir, `${name}.pcm`),
    file,
  );

  const seconds = await probeDuration(file);
  const { words, expected, ceiling } = durationCeiling(text);
  if (seconds > ceiling) {
    throw new Error(
      `Gemini TTS ran ${seconds.toFixed(0)}s for ${words} words ` +
        `(expected ~${expected.toFixed(0)}s):\n  ${text}`,
    );
  }
  return { file, seconds };
}

/** One streamed gpt-audio call: raw PCM plus what it says it said. */
async function streamGptAudio(text: string) {
  const res = await fetch("https://openrouter.ai/api/v1/chat/completions", {
    method: "POST",
    headers: {
      Authorization: `Bearer ${process.env.OPENROUTER_API_KEY}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      model: "openai/gpt-audio",
      // Audio output is streaming-only on chat completions.
      stream: true,
      temperature: 0,
      modalities: ["text", "audio"],
      audio: { voice: gptAudioVoice(), format: "pcm16" },
      messages: [
        { role: "system", content: VERBATIM_SYSTEM },
        { role: "user", content: text },
      ],
    }),
  });
  if (!res.ok || !res.body) {
    throw new Error(`gpt-audio ${res.status}: ${(await res.text()).slice(0, 300)}`);
  }

  const chunks: Buffer[] = [];
  let transcript = "";
  let buffered = "";
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buffered += decoder.decode(value, { stream: true });
    const lines = buffered.split("\n");
    buffered = lines.pop() ?? "";
    for (const line of lines) {
      if (!line.startsWith("data: ")) continue;
      const payload = line.slice(6).trim();
      if (!payload || payload === "[DONE]") continue;
      try {
        const event = JSON.parse(payload);
        const audio = event.choices?.[0]?.delta?.audio;
        if (audio?.data) chunks.push(Buffer.from(audio.data, "base64"));
        if (audio?.transcript) transcript += audio.transcript;
      } catch {
        // Keep-alive comments and partial frames; ignore.
      }
    }
  }
  return { pcm: Buffer.concat(chunks), transcript };
}

async function synthesiseGptAudio(
  text: string,
  outDir: string,
  name: string,
): Promise<NarrationClip> {
  const raw = path.join(outDir, `${name}.pcm`);
  const file = path.join(outDir, `${name}.wav`);
  const want = normalise(text);
  const { words, expected, ceiling } = durationCeiling(text);

  let why = "never ran";
  for (let attempt = 1; attempt <= 3; attempt++) {
    const { pcm, transcript } = await streamGptAudio(text);
    const heard = transcript.trim();

    if (!pcm.length) {
      why = "returned no audio";
    } else if (normalise(heard) !== want) {
      why = `read something else: "${heard.slice(0, 80)}"`;
    } else {
      await pcmToWav(pcm, raw, file);
      const seconds = await probeDuration(file);
      if (seconds <= ceiling) return { file, seconds };
      why = `ran ${seconds.toFixed(0)}s for ${words} words (expected ~${expected.toFixed(0)}s)`;
    }
    console.warn(`  ! retry ${attempt}/3 — ${why}`);
  }

  throw new Error(
    `gpt-audio could not narrate this line cleanly after 3 tries (${why}).\n` +
      `  line: ${text}`,
  );
}

async function synthesiseSay(
  text: string,
  outDir: string,
  name: string,
): Promise<NarrationClip> {
  const file = path.join(outDir, `${name}.aiff`);
  await rm(file, { force: true });
  await run("say", ["-v", sayVoice(), "-r", String(sayRate()), "-o", file, text]);
  return { file, seconds: await probeDuration(file) };
}

/**
 * Synthesise one narration line. Returns the measured duration rather than an
 * estimate from the word count — the scheduler holds each shot on screen for
 * exactly as long as its line takes to speak, so a guess would drift.
 */
export async function synthesise(
  text: string,
  outDir: string,
  name: string,
): Promise<NarrationClip> {
  await mkdir(outDir, { recursive: true });
  switch (backend()) {
    case "gemini":
      return synthesiseGemini(text, outDir, name);
    case "gptaudio":
      return synthesiseGptAudio(text, outDir, name);
    default:
      return synthesiseSay(text, outDir, name);
  }
}

/** Human-readable description of what will do the talking. */
export function describeVoice(): string {
  switch (backend()) {
    case "gemini":
      return `${geminiModel()} (${geminiVoice()})`;
    case "gptaudio":
      return `openai/gpt-audio (${gptAudioVoice()})`;
    default:
      return `macOS say (${sayVoice()})`;
  }
}

/** Fail early with an actionable message rather than mid-render. */
export async function assertVoiceAvailable(): Promise<void> {
  if (backend() !== "say") {
    if (!process.env.OPENROUTER_API_KEY) {
      throw new Error(
        `VIDEO_TTS=${backend()} needs OPENROUTER_API_KEY. It lives in .env — ` +
          "the entry in .env.local is an empty placeholder. Or set " +
          "VIDEO_TTS=say to use the macOS voice.",
      );
    }
    return;
  }

  const { stdout } = await run("say", ["-v", "?"]);
  // Each line is "Name<2+ spaces>locale<2+ spaces># sample".
  const rows = stdout
    .split("\n")
    .map((line) => line.split(/\s{2,}/))
    .filter((cols) => cols.length >= 2)
    .map((cols) => ({ name: cols[0].trim(), locale: cols[1].trim() }));

  if (rows.some((r) => r.name === sayVoice())) return;

  throw new Error(
    `Voice "${sayVoice()}" is not installed. Set VIDEO_VOICE_SAY to one of:\n  ` +
      rows.filter((r) => r.locale.startsWith("en")).map((r) => r.name).join(", "),
  );
}
