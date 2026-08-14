export type AddonRole = 'video' | 'audio' | 'both';

export interface Service {
  id: string;
  name: string;
  shortName: string;
  credFields: { id: string; name: string }[];
}

export interface ServiceConfig {
  id: string;
  enabled: boolean;
  credentials: Record<string, string>;
}

export interface Addon {
  id: string;
  name: string;
  manifestUrl: string;
  role: AddonRole;
  language: string;
  enabled: boolean;
  timeout?: number;
}

export interface Config {
  language: string;
  services: ServiceConfig[];
  addons: Addon[];
}

export const DEFAULT_CONFIG: Config = {
  language: 'Portuguese (Brazil)',
  services: [],
  addons: [],
};

export const LANGUAGES = [
  'Portuguese (Brazil)',
  'Portuguese',
  'English',
  'Spanish',
  'French',
  'German',
  'Italian',
  'Japanese',
  'Korean',
  'Hindi',
];

export const ROLE_LABELS: Record<AddonRole, { label: string; description: string }> = {
  video: {
    label: 'Vídeo',
    description: 'Fonte de melhor qualidade de imagem (geralmente inglês)',
  },
  audio: {
    label: 'Áudio',
    description: 'Fonte de áudio no seu idioma (geralmente dublado)',
  },
  both: {
    label: 'Vídeo + Áudio',
    description: 'Pode ser usado para os dois (se dublado com ótima qualidade)',
  },
};

export const SERVICES: Service[] = [
  { id: 'realdebrid', name: 'Real-Debrid', shortName: 'RD', credFields: [{ id: 'apiKey', name: 'API Key' }] },
  { id: 'torbox', name: 'TorBox', shortName: 'TB', credFields: [{ id: 'apiKey', name: 'API Key' }] },
  { id: 'alldebrid', name: 'AllDebrid', shortName: 'AD', credFields: [{ id: 'apiKey', name: 'API Key' }] },
  { id: 'premiumize', name: 'Premiumize', shortName: 'PM', credFields: [{ id: 'apiKey', name: 'API Key' }] },
  { id: 'debridlink', name: 'Debrid-Link', shortName: 'DL', credFields: [{ id: 'apiKey', name: 'API Key' }] },
];
