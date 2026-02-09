/**
 * Serial Poller Composable
 * Provides serial polling with:
 * - No overlapping requests (only one in-flight at a time)
 * - Stale response protection via generation counter
 * - AbortController for cancellation
 * - 429 Retry-After handling with UI state
 * - Automatic cleanup on scope dispose
 */

import { ref, computed, onScopeDispose, unref, type Ref, type ComputedRef } from 'vue'
import axios from 'axios'

interface ApiClientError {
  status?: number
  retryAfter?: number
}

export type SerialPollerConcurrency = 'abortPrevious' | 'ignoreIfRunning'

export interface SerialPollerOptions<T> {
  /** Polling interval in milliseconds */
  intervalMs: number
  /** Whether to run immediately on start (default: false) */
  immediate?: boolean
  /** Reactive enable flag - if false, poller will not schedule new work */
  enabled?: Ref<boolean> | boolean
  /** Concurrency behavior for runNow() when request is in-flight (default: 'abortPrevious') */
  concurrency?: SerialPollerConcurrency
  /** Default retry delay if Retry-After header is missing (default: 1000ms) */
  defaultRetryAfterMs?: number
  /** Maximum retry delay to prevent infinite waits (default: 60000ms) */
  maxRetryAfterMs?: number
  /** Called with successful data */
  onData?: (data: T) => void
  /** Called on any error */
  onError?: (error: unknown) => void
}

export interface SerialPollerContext {
  signal: AbortSignal
  generation: number
}

export interface SerialPoller<T> {
  /** Latest successful data */
  data: Ref<T | null>
  /** Latest error (null if last request succeeded) */
  error: Ref<unknown | null>
  /** True while request is in-flight */
  loading: Ref<boolean>
  /** True if poller is actively running (scheduled) */
  isRunning: Ref<boolean>
  /** True while request is in-flight */
  inFlight: Ref<boolean>
  /** True if 429 rate limit hit and waiting for retry */
  busy: Ref<boolean>
  /** Timestamp (ms) when retry will happen after 429 */
  busyUntil: Ref<number | null>
  /** Milliseconds until next retry (reactive, for countdown display) */
  retryingInMs: ComputedRef<number | null>
  /** Start the polling loop */
  start: () => void
  /** Stop the polling loop and cancel any in-flight request */
  stop: () => void
  /** Trigger an immediate poll (returns result or undefined if aborted/errored) */
  runNow: () => Promise<T | undefined>
}

/**
 * Create a serial poller that executes a fetcher function on an interval.
 * Only one request runs at a time. Stale responses are ignored.
 *
 * @param fetcher - Async function that returns data. Receives { signal, generation } context.
 * @param options - Polling configuration
 */
export function useSerialPoller<T>(
  fetcher: (ctx: SerialPollerContext) => Promise<T>,
  options: SerialPollerOptions<T>
): SerialPoller<T> {
  const data = ref<T | null>(null) as Ref<T | null>
  const error = ref<unknown | null>(null)
  const loading = ref(false)

  const isRunning = ref(false)
  const inFlight = ref(false)

  const busy = ref(false)
  const busyUntil = ref<number | null>(null)

  const retryingInMs = computed(() => {
    if (!busyUntil.value) return null
    return Math.max(0, busyUntil.value - Date.now())
  })

  let timer: ReturnType<typeof setTimeout> | null = null
  let generation = 0
  let controller: AbortController | null = null

  const isEnabled = () => (options.enabled === undefined ? true : !!unref(options.enabled))
  const concurrency: SerialPollerConcurrency = options.concurrency ?? 'abortPrevious'

  const clearTimer = () => {
    if (timer) {
      clearTimeout(timer)
      timer = null
    }
  }

  const abortInFlight = () => {
    if (controller) {
      controller.abort()
      controller = null
    }
  }

  const schedule = (delayMs: number) => {
    clearTimer()
    if (!isRunning.value) return
    if (!isEnabled()) return
    timer = setTimeout(() => void runOnce(), delayMs)
  }

  const parseRetryAfterMs = (err: unknown): number | null => {
    const e = err as ApiClientError | undefined
    const v = e?.retryAfter
    return typeof v === 'number' && Number.isFinite(v) ? v : null
  }

  const clampRetryAfter = (ms: number): number => {
    const max = options.maxRetryAfterMs ?? 60_000
    return Math.max(0, Math.min(ms, max))
  }

  const runOnce = async (): Promise<T | undefined> => {
    if (!isEnabled()) return

    if (inFlight.value) {
      if (concurrency === 'ignoreIfRunning') return
      abortInFlight()
    }

    const currentGen = ++generation
    controller = new AbortController()

    inFlight.value = true
    loading.value = true
    busy.value = false
    busyUntil.value = null
    error.value = null

    try {
      const result = await fetcher({ signal: controller.signal, generation: currentGen })
      // Stale protection: only update if this is still the latest generation
      if (currentGen === generation) {
        data.value = result
        options.onData?.(result)
      }
      return result
    } catch (e: unknown) {
      // Abort/cancel: do not surface as "error"
      const axiosError = e as { code?: string }
      if (
        axiosError?.code === 'ERR_CANCELED' ||
        axios.isCancel?.(e) ||
        controller?.signal.aborted
      ) {
        return
      }

      error.value = e
      options.onError?.(e)

      // 429 handling: schedule retry with Retry-After delay
      const apiError = e as ApiClientError | undefined
      if (apiError?.status === 429) {
        const retryAfterMs = clampRetryAfter(
          parseRetryAfterMs(e) ?? (options.defaultRetryAfterMs ?? 1000)
        )
        busy.value = true
        busyUntil.value = Date.now() + retryAfterMs
        schedule(retryAfterMs)
        return
      }

      return
    } finally {
      // Only clear loading state if we're still the latest generation
      if (currentGen === generation) {
        inFlight.value = false
        loading.value = false
      }
      // Schedule next poll if running and not in 429 retry mode
      if (isRunning.value && isEnabled() && !busy.value) {
        schedule(options.intervalMs)
      }
    }
  }

  const start = () => {
    if (isRunning.value) return
    isRunning.value = true
    if (options.immediate) {
      void runOnce()
    } else {
      schedule(options.intervalMs)
    }
  }

  const stop = () => {
    isRunning.value = false
    clearTimer()
    abortInFlight()
    inFlight.value = false
    busy.value = false
    busyUntil.value = null
  }

  const runNow = async () => runOnce()

  // Cleanup on scope dispose (component unmount, etc.)
  onScopeDispose(stop)

  return {
    data,
    error,
    loading,
    isRunning,
    inFlight,
    busy,
    busyUntil,
    retryingInMs,
    start,
    stop,
    runNow
  }
}
