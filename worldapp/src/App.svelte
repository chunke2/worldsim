<script>
  import { onMount } from 'svelte';
  import { get } from 'svelte/store';
  import { activeTab, commandOpen, restore, openTab, switchTab, tabs } from './lib/browser.js';
  import { theme, applyTheme } from './lib/theme.js';
  import { toasts } from './lib/api.js';
  import BrowserChrome from './components/BrowserChrome.svelte';
  import CommandPalette from './components/CommandPalette.svelte';
  import StatusBar from './components/StatusBar.svelte';
  import BookmarkBar from './components/BookmarkBar.svelte';
  import { ROUTES } from './lib/routes.js';

  onMount(() => {
    applyTheme($theme);
    restore();
    // 启动时始终优先打开/激活开始页面。
    // 浏览器仍保留其他标签，但不再因为上次停在小说页而直接进入小说创作。
    const restoredTabs = get(tabs);
    const homeTab = restoredTabs.find((tab) => tab.address === 'home');
    if (homeTab) switchTab(homeTab.id);
    else openTab('home');

    // 键盘快捷键
    const onKey = (e) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        commandOpen.set(true);
      } else if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 't' && !$activeTab) {
        e.preventDefault();
        openTab('home');
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  });
</script>

<svelte:head>
  <title>WorldSim 统一前端</title>
</svelte:head>

<div class="browser-shell bg-base-100 text-base-content">
  <BrowserChrome />
  <BookmarkBar />

  <div class="browser-viewport">
    {#if $activeTab}
      {#key $activeTab.id}
        <svelte:component this={ROUTES[$activeTab.address]?.component || ROUTES.home.component} class="page-enter h-full" />
      {/key}
    {:else}
      <div class="flex items-center justify-center h-full opacity-50">无打开的标签页</div>
    {/if}
  </div>

  <!-- Toasts -->
  <div class="fixed top-5 right-5 z-50 flex flex-col gap-2">
    {#each $toasts as t (t.id)}
      <div class="alert alert-sm {t.type === 'success' ? 'alert-success' : t.type === 'error' ? 'alert-error' : 'alert-info'} shadow-lg max-w-sm page-enter">
        <span>{t.msg}</span>
      </div>
    {/each}
  </div>

  <CommandPalette />
  <StatusBar />
</div>
