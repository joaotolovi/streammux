export interface Envelope<T> {
  success: boolean;
  data: T;
  error?: { message: string };
}

export async function api<T>(endpoint: string, options: RequestInit = {}): Promise<T> {
  const res = await fetch(`/api/v1${endpoint}`, {
    credentials: 'include',
    headers: { 'Content-Type': 'application/json', ...(options.headers || {}) },
    ...options,
  });
  let body: Envelope<T>;
  try {
    body = await res.json();
  } catch {
    throw new Error(`Request failed (${res.status})`);
  }
  if (!res.ok) {
    throw new Error(body?.error?.message || `Request failed (${res.status})`);
  }
  return body.data;
}
