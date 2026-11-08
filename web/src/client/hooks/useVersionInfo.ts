import { useEffect, useState } from 'react';

export interface VersionInfo {
  name: string;
  version: string;
}

// Fetches the server's name and version (shown so bug reports can be told
// apart from upstream difit) once enabled is true.
export function useVersionInfo(enabled: boolean): VersionInfo | null {
  const [info, setInfo] = useState<VersionInfo | null>(null);
  useEffect(() => {
    if (!enabled || info) return;
    const load = async () => {
      try {
        const response = await fetch('/api/version');
        if (response.ok) setInfo((await response.json()) as VersionInfo);
      } catch {
        // The version is informational; leave it out if it can't be loaded.
      }
    };
    void load();
  }, [enabled, info]);
  return info;
}
