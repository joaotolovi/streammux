import { useEffect, useState } from 'react';
import { Button } from '@/components/base/buttons/button';
import { Input } from '@/components/base/input/input';
import { InputNumber } from '@/components/base/input/input-number';
import { Select } from '@/components/base/select/select';
import { Toggle } from '@/components/base/toggle/toggle';
import { Badge } from '@/components/base/badges/badges';
import { Tooltip, TooltipTrigger } from '@/components/base/tooltip/tooltip';
import {
  Clapperboard,
  Plus,
  Check,
  X,
  Copy04,
  Download01,
  Key01,
  ChevronDown,
  ChevronRight,
  Headphones01,
  Tv01,
  Globe02,
  Film01,
  Settings01,
  Trash01,
  Edit02,
  AlertCircle,
  InfoCircle,
  MusicNote01,
} from '@untitledui/icons';
import {
  Addon,
  ADDON_LANGUAGE_OPTIONS,
  AddonRole,
  Config,
  DEFAULT_CONFIG,
  LANGUAGES,
  ROLE_LABELS,
  SERVICES,
  ServiceConfig,
} from './types';

export default function App() {
  const [config, setConfig] = useState<Config>(() => {
    const saved = localStorage.getItem('streammux-config');
    return saved ? { ...DEFAULT_CONFIG, ...JSON.parse(saved) } : DEFAULT_CONFIG;
  });
  const [password, setPassword] = useState('');
  const [uuid, setUuid] = useState('');
  const [encryptedPassword, setEncryptedPassword] = useState('');
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState('');

  if (window.location.pathname.startsWith('/admin')) {
    return <AdminPanel />;
  }

  const set = (patch: Partial<Config>) => {
    setConfig((prev) => ({ ...prev, ...patch }));
    setSaved(false);
    setError('');
  };

  const save = async () => {
    if (!password) return;
    setError('');
    try {
      const res = await fetch('/api/v1/user', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ config, password }),
      });
      const json = await res.json();
      if (!res.ok) {
        setError(json.error?.message || `Request failed (${res.status})`);
        return;
      }
      setUuid(json.uuid);
      setEncryptedPassword(json.encryptedPassword);
      setSaved(true);
      localStorage.setItem('streammux-config', JSON.stringify(config));
    } catch (e) {
      setError((e as Error).message);
    }
  };

  const installUrl =
    uuid && encryptedPassword
      ? `${window.location.origin}/stremio/${uuid}/${encryptedPassword}/manifest.json`
      : '';

  return (
    <div className="min-h-screen bg-[var(--color-bg-primary)] text-[var(--color-text-primary)]">
      <Header saved={saved} onSave={save} hasPassword={!!password} />
      <main className="mx-auto max-w-3xl px-4 py-8 sm:px-6">
        <div className="mb-8 text-center">
          <div className="mb-4 inline-flex items-center gap-2 rounded-full bg-brand-500/10 px-4 py-1.5 text-sm font-medium text-brand-600 ring-1 ring-brand-500/20">
            <Clapperboard className="size-4" />
            Addon de muxing de áudio e vídeo
          </div>
          <h1 className="text-3xl font-semibold tracking-tight sm:text-4xl">
            Melhor imagem. Seu idioma.
          </h1>
          <p className="mx-auto mt-3 max-w-xl text-md text-[var(--color-text-tertiary)]">
            O StreamMux combina o vídeo da maior qualidade (geralmente em inglês)
            com o áudio dublado no seu idioma — entregando o melhor dos dois mundos
            em um único stream.
          </p>
        </div>

        <div className="space-y-6">
          <LanguageSection value={config.language} onChange={(language) => set({ language })} />

          <ServicesSection
            services={config.services}
            onChange={(services) => set({ services })}
          />

          <AddonsSection
            addons={config.addons}
            onChange={(addons) => set({ addons })}
          />

          <SaveSection
            password={password}
            setPassword={setPassword}
            onSave={save}
            saved={saved}
            error={error}
            installUrl={installUrl}
          />
        </div>
      </main>
    </div>
  );
}

function AdminPanel() {
  const [configured, setConfigured] = useState(false);
  const [authenticated, setAuthenticated] = useState(false);
  const [authPassword, setAuthPassword] = useState('');
  const [config, setConfig] = useState<Config>(DEFAULT_CONFIG);
  const [installUrl, setInstallUrl] = useState('');
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(true);

  const refresh = async () => {
    const response = await fetch('/api/v1/admin/status');
    const status = await response.json();
    if (!response.ok) throw new Error(status.error?.message || 'Admin indisponível');
    setConfigured(status.configured);
    setAuthenticated(status.authenticated);
    if (status.authenticated) {
      const configResponse = await fetch('/api/v1/admin/config');
      const data = await configResponse.json();
      if (!configResponse.ok) throw new Error(data.error?.message || 'Configuração indisponível');
      setConfig({ ...DEFAULT_CONFIG, ...data.config });
      if (data.uuid && data.encryptedPassword) {
        setInstallUrl(`${window.location.origin}/stremio/${data.uuid}/${data.encryptedPassword}/manifest.json`);
      }
    }
  };

  useEffect(() => {
    refresh().catch((e) => setError((e as Error).message)).finally(() => setLoading(false));
  }, []);

  const authenticate = async () => {
    setError('');
    try {
      const endpoint = configured ? '/api/v1/admin/login' : '/api/v1/admin/setup';
      const response = await fetch(endpoint, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password: authPassword }),
      });
      const data = await response.json();
      if (!response.ok) {
        setError(data.error?.message || 'Senha inválida');
        return;
      }
      setAuthPassword('');
      await refresh();
    } catch (e) {
      setError((e as Error).message);
    }
  };

  const set = (patch: Partial<Config>) => {
    setConfig((previous) => ({ ...previous, ...patch }));
    setSaved(false);
    setError('');
  };

  const save = async () => {
    setError('');
    const response = await fetch('/api/v1/admin/config', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ config }),
    });
    const data = await response.json();
    if (!response.ok) {
      setError(data.error?.message || 'Não foi possível salvar');
      return;
    }
    setInstallUrl(`${window.location.origin}/stremio/${data.uuid}/${data.encryptedPassword}/manifest.json`);
    setSaved(true);
  };

  if (loading) {
    return <AdminShell><p className="text-sm text-[var(--color-text-tertiary)]">Carregando painel...</p></AdminShell>;
  }
  if (!authenticated) {
    return (
      <AdminShell>
        <div className="mx-auto max-w-md">
          <SectionCard
            icon={<Settings01 className="size-5" />}
            title={configured ? 'Acesso administrativo' : 'Configurar acesso administrativo'}
            description={configured ? 'Informe a senha para acessar a configuração do addon.' : 'Defina uma senha com pelo menos 8 caracteres para proteger este painel.'}
          >
            <div className="space-y-4">
              <Input
                label="Senha administrativa"
                type="password"
                value={authPassword}
                onChange={(value) => setAuthPassword(String(value))}
                onKeyDown={(event) => { if (event.key === 'Enter') authenticate(); }}
              />
              <Button color="primary" onPress={authenticate} isDisabled={authPassword.length < 8}>
                {configured ? 'Entrar' : 'Criar acesso'}
              </Button>
              {error && <p className="text-sm text-[var(--color-text-error-primary)]">{error}</p>}
            </div>
          </SectionCard>
        </div>
      </AdminShell>
    );
  }

  return (
    <AdminShell saved={saved} onSave={save}>
      <div className="space-y-6">
        <LanguageSection value={config.language || DEFAULT_CONFIG.language} onChange={(language) => set({ language })} />
        <ServicesSection services={config.services || []} onChange={(services) => set({ services })} />
        <AddonsSection addons={config.addons || []} onChange={(addons) => set({ addons })} />
        <SaveSection password="configured" setPassword={() => undefined} onSave={save} saved={saved} error={error} installUrl={installUrl} admin />
      </div>
    </AdminShell>
  );
}

function AdminShell({ children, saved, onSave }: { children: React.ReactNode; saved?: boolean; onSave?: () => void }) {
  return (
    <div className="min-h-screen bg-[var(--color-bg-primary)] text-[var(--color-text-primary)]">
      <Header saved={saved ?? false} onSave={onSave ?? (() => undefined)} hasPassword={!!onSave} admin />
      <main className="mx-auto max-w-3xl px-4 py-8 sm:px-6">
        <div className="mb-8 text-center">
          <div className="mb-4 inline-flex items-center gap-2 rounded-full bg-brand-500/10 px-4 py-1.5 text-sm font-medium text-brand-600 ring-1 ring-brand-500/20">
            <Settings01 className="size-4" />
            Administração do StreamMux
          </div>
          <h1 className="text-3xl font-semibold tracking-tight sm:text-4xl">Configuração protegida</h1>
          <p className="mx-auto mt-3 max-w-xl text-md text-[var(--color-text-tertiary)]">Gerencie addons, serviços e idioma do addon sem expor suas credenciais publicamente.</p>
        </div>
        {children}
      </main>
    </div>
  );
}

function Header({ saved, onSave, hasPassword, admin = false }: { saved: boolean; onSave: () => void; hasPassword: boolean; admin?: boolean }) {
  return (
    <header className="sticky top-0 z-10 border-b border-[var(--color-border-secondary)] bg-[var(--color-bg-primary)]/80 backdrop-blur">
      <div className="mx-auto flex h-16 max-w-3xl items-center justify-between px-4 sm:px-6">
        <div className="flex items-center gap-2.5">
          <div className="flex size-9 items-center justify-center rounded-lg bg-brand-600 text-white">
            <Clapperboard className="size-5" />
          </div>
          <span className="text-lg font-semibold">StreamMux</span>
        </div>
        <div className="flex items-center gap-3">
          {saved && (
            <span className="flex items-center gap-1.5 text-sm font-medium text-success-600">
              <Check className="size-4" />
              Salvo
            </span>
          )}
          <Button color="primary" size="md" isDisabled={!hasPassword} onPress={onSave}>
            {admin ? 'Salvar configuração' : 'Salvar'}
          </Button>
          {!admin && (
            <a href="/admin" className="text-sm font-medium text-[var(--color-text-tertiary)] hover:text-[var(--color-text-primary)]">
              Administração
            </a>
          )}
        </div>
      </div>
    </header>
  );
}

function LanguageSection({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <SectionCard
      icon={<Globe02 className="size-5" />}
      title="Idioma preferido"
      description="O idioma do áudio que você quer (geralmente a versão dublada)."
    >
      <Select
        label="Idioma do áudio"
        selectedKey={value}
        onSelectionChange={(k) => onChange(String(k))}
      >
        {LANGUAGES.map((lang) => (
          <Select.Item key={lang} id={lang}>
            {lang}
          </Select.Item>
        ))}
      </Select>
    </SectionCard>
  );
}

function ServicesSection({
  services,
  onChange,
}: {
  services: ServiceConfig[];
  onChange: (s: ServiceConfig[]) => void;
}) {
  const toggleService = (id: string) => {
    const existing = services.find((s) => s.id === id);
    if (existing) {
      onChange(services.map((s) => (s.id === id ? { ...s, enabled: !s.enabled } : s)));
    } else {
      onChange([...services, { id, enabled: true, credentials: {} }]);
    }
  };
  const setCred = (id: string, credId: string, v: string) => {
    onChange(
      services.map((s) =>
        s.id === id ? { ...s, credentials: { ...s.credentials, [credId]: v } } : s
      )
    );
  };

  return (
    <SectionCard
      icon={<Key01 className="size-5" />}
      title="Serviços de Debrid"
      description="Quando um addon retorna torrent não resolvido (só infoHash), o StreamMux resolve via seu debrid para obter a URL direta."
    >
      <div className="space-y-3">
        {SERVICES.map((svc) => {
          const svcData = services.find((s) => s.id === svc.id);
          const enabled = !!svcData?.enabled;
          return (
            <div
              key={svc.id}
              className="rounded-xl border border-[var(--color-border-secondary)] p-4"
            >
              <div className="flex items-center justify-between">
                <div className="flex items-center gap-3">
                  <div className="flex size-10 items-center justify-center rounded-lg bg-[var(--color-bg-secondary)] text-sm font-semibold">
                    {svc.shortName}
                  </div>
                  <div>
                    <p className="font-medium">{svc.name}</p>
                    <p className="text-sm text-[var(--color-text-tertiary)]">
                      {enabled ? 'Conectado' : 'Desconectado'}
                    </p>
                  </div>
                </div>
                <Toggle
                  isSelected={enabled}
                  onChange={() => toggleService(svc.id)}
                />
              </div>
              {enabled && (
                <div className="mt-4 space-y-3 border-t border-[var(--color-border-secondary)] pt-4">
                  {svc.credFields.map((field) => (
                    <Input
                      key={field.id}
                      label={field.name}
                      placeholder={`Chave de API do ${svc.name}`}
                      type="password"
                      value={svcData?.credentials[field.id] || ''}
                      onChange={(v) => setCred(svc.id, field.id, String(v))}
                    />
                  ))}
                </div>
              )}
            </div>
          );
        })}
      </div>
    </SectionCard>
  );
}

function AddonsSection({
  addons,
  onChange,
}: {
  addons: Addon[];
  onChange: (a: Addon[]) => void;
}) {
  const addAddon = () => {
    onChange([
      ...addons,
      {
        id: `addon-${Date.now()}`,
        name: '',
        manifestUrl: '',
        role: 'both',
        language: '',
        enabled: true,
        timeout: 20000,
        showAllAudioLanguages: false,
        audioLanguages: [],
      },
    ]);
  };

  const updateAddon = (id: string, patch: Partial<Addon>) => {
    onChange(addons.map((a) => (a.id === id ? { ...a, ...patch } : a)));
  };

  const removeAddon = (id: string) => {
    onChange(addons.filter((a) => a.id !== id));
  };

  return (
    <SectionCard
      icon={<Film01 className="size-5" />}
      title="Addons de origem"
      description="Cadastre os addons que fornecem streams. Cada um pode ser fonte de vídeo, de áudio, ou ambos."
      action={
        <Button color="secondary" size="sm" onPress={addAddon}>
          <Plus className="size-4" />
          Adicionar addon
        </Button>
      }
    >
      {addons.length === 0 ? (
        <div className="flex flex-col items-center gap-3 rounded-xl border border-dashed border-[var(--color-border-secondary)] py-10 text-center">
          <Plus className="size-8 text-[var(--color-text-quaternary)]" />
          <p className="max-w-xs text-sm text-[var(--color-text-tertiary)]">
            Nenhum addon cadastrado. Adicione addons como Torrentio, Comet ou
            MediaFusion para começar.
          </p>
          <Button color="secondary" size="sm" onPress={addAddon}>
            <Plus className="size-4" />
            Adicionar addon
          </Button>
        </div>
      ) : (
        <div className="space-y-3">
          {addons.map((addon) => (
            <AddonCard
              key={addon.id}
              addon={addon}
              onChange={(patch) => updateAddon(addon.id, patch)}
              onRemove={() => removeAddon(addon.id)}
            />
          ))}
        </div>
      )}
    </SectionCard>
  );
}

function AddonCard({
  addon,
  onChange,
  onRemove,
}: {
  addon: Addon;
  onChange: (patch: Partial<Addon>) => void;
  onRemove: () => void;
}) {
  return (
    <div className="rounded-xl border border-[var(--color-border-secondary)] p-4">
      <div className="flex items-start justify-between gap-3">
        <div className="flex-1 space-y-3">
          <div className="flex items-center gap-2">
            <Badge color={roleColor(addon.role)}>{ROLE_LABELS[addon.role].label}</Badge>
            <Toggle
              isSelected={addon.enabled}
              onChange={(v) => onChange({ enabled: v })}
              slim
            />
          </div>
          <Input
            label="Nome"
            placeholder="Ex: Torrentio"
            value={addon.name}
            onChange={(v) => onChange({ name: String(v) })}
          />
          <Input
            label="Manifest URL"
            placeholder="https://.../manifest.json"
            value={addon.manifestUrl}
            onChange={(v) => onChange({ manifestUrl: String(v) })}
          />
          <div className="grid gap-3 sm:grid-cols-2">
            <Select
              label="Função"
              selectedKey={addon.role}
              onSelectionChange={(k) => onChange({ role: k as AddonRole })}
            >
              {(Object.keys(ROLE_LABELS) as AddonRole[]).map((role) => (
                <Select.Item key={role} id={role}>
                  {ROLE_LABELS[role].label}
                </Select.Item>
              ))}
            </Select>
            <Select
              label="Idioma"
              tooltip="Força o idioma dos streams deste addon. Deixe 'Indefinido' para detectar automaticamente pelo nome do arquivo (bandeiras, 'Dublado', etc.)."
              selectedKey={addon.language}
              onSelectionChange={(k) => onChange({ language: String(k) })}
            >
              {ADDON_LANGUAGE_OPTIONS.map((opt) => (
                <Select.Item key={opt.value || 'unset'} id={opt.value}>
                  {opt.label}
                </Select.Item>
              ))}
            </Select>
            <InputNumber
              label="Timeout (s)"
              value={(addon.timeout ?? 20000) / 1000}
              minValue={1}
              maxValue={120}
              step={1}
              onChange={(v) => onChange({ timeout: Math.round((v ?? 20) * 1000) })}
            />
            <div className="sm:col-span-2 space-y-2">
              <Toggle
                isSelected={addon.showAllAudioLanguages ?? false}
                onChange={(v) => onChange({ showAllAudioLanguages: v })}
                label="Mostrar todas as línguas encontradas"
              />
              <Input
                label="Filtrar línguas (opcional)"
                placeholder="English, Spanish, French"
                tooltip="O idioma principal do usuário sempre aparece. Separe outras línguas por vírgula. Ignorado quando 'todas' estiver ativo."
                value={(addon.audioLanguages ?? []).join(', ')}
                onChange={(v) =>
                  onChange({
                    audioLanguages: String(v)
                      .split(',')
                      .map((language) => language.trim())
                      .filter(Boolean),
                  })
                }
              />
            </div>
          </div>
        </div>
        <Tooltip title="Remover addon">
          <TooltipTrigger onPress={onRemove}>
            <button className="rounded-lg p-2 text-[var(--color-text-quaternary)] transition hover:bg-error-500/10 hover:text-error-600">
              <Trash01 className="size-5" />
            </button>
          </TooltipTrigger>
        </Tooltip>
      </div>
    </div>
  );
}

function roleColor(role: AddonRole): 'brand' | 'success' | 'purple' {
  switch (role) {
    case 'video':
      return 'brand';
    case 'audio':
      return 'success';
    default:
      return 'purple';
  }
}

function SaveSection({
  password,
  setPassword,
  onSave,
  saved,
  error,
  installUrl,
  admin = false,
}: {
  password: string;
  setPassword: (v: string) => void;
  onSave: () => void;
  saved: boolean;
  error: string;
  installUrl: string;
  admin?: boolean;
}) {
  return (
    <SectionCard
      icon={<Settings01 className="size-5" />}
      title="Salvar & Instalar"
      description={admin ? 'A configuração é protegida pela senha administrativa e pode ser atualizada a qualquer momento.' : 'Proteja sua configuração com uma senha e instale no Stremio.'}
    >
      {!admin && <Input
        label="Senha"
        type="password"
        placeholder="Escolha uma senha"
        value={password}
        onChange={(v) => setPassword(String(v))}
      />}
      <div className="flex gap-3">
        <Button color="primary" size="md" isDisabled={!admin && !password} onPress={onSave}>
          Salvar configuração
        </Button>
      </div>
      {error && (
        <p className="text-sm text-[var(--color-text-error-primary)]">{error}</p>
      )}
      {saved && installUrl && (
        <div className="rounded-xl border border-[var(--color-border-secondary)] bg-[var(--color-bg-secondary)] p-4">
          <p className="mb-3 font-medium">Instale no Stremio</p>
          <div className="flex flex-col gap-2">
            <Input value={installUrl} isReadOnly />
            <div className="flex flex-wrap gap-2">
              <Button
                color="secondary"
                size="md"
                onPress={() => navigator.clipboard.writeText(installUrl)}
              >
                <Copy04 className="size-4" />
                Copiar URL
              </Button>
              <Button
                color="primary"
                size="md"
                onPress={() => window.open(`stremio://${installUrl.replace(/^https?:\/\//, '')}`, '_self')}
              >
                <Download01 className="size-4" />
                Instalar no Stremio
              </Button>
            </div>
          </div>
        </div>
      )}
    </SectionCard>
  );
}

function SectionCard({
  icon,
  title,
  description,
  action,
  children,
}: {
  icon: React.ReactNode;
  title: string;
  description: string;
  action?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <section className="rounded-2xl border border-[var(--color-border-secondary)] bg-[var(--color-bg-primary)] p-5 shadow-xs">
      <div className="mb-4 flex items-start justify-between gap-4">
        <div className="flex items-start gap-3">
          <div className="mt-0.5 flex size-9 shrink-0 items-center justify-center rounded-lg bg-[var(--color-bg-secondary)] text-[var(--color-fg-brand-primary)]">
            {icon}
          </div>
          <div>
            <h2 className="text-lg font-semibold">{title}</h2>
            <p className="text-sm text-[var(--color-text-tertiary)]">{description}</p>
          </div>
        </div>
        {action}
      </div>
      {children}
    </section>
  );
}
