<template>
  <el-config-provider :locale="elementLocale">
    <ShellView v-if="route === 'shell'" />
    <div v-else class="app-shell">
      <header class="topbar">
        <div class="brand-block">
          <img class="brand-mark" :src="iconPath" alt="ChemSSH" />
          <div>
            <h1>{{ appTitle }}</h1>
            <p>{{ subtitle }}</p>
          </div>
        </div>
        <div class="topbar-actions">
          <div class="language-toggle" :aria-label="t('language.switch')" role="group">
            <button type="button" :class="{ 'is-active': locale === 'zh' }" @click="setLocale('zh')">{{ t('language.zh') }}</button>
            <button type="button" :class="{ 'is-active': locale === 'en' }" @click="setLocale('en')">EN</button>
          </div>
          <el-segmented v-model="route" :options="navOptions" />
        </div>
      </header>
      <main class="app-main">
        <LauncherView v-show="route === 'launcher'" />
        <SftpView v-show="route === 'sftp'" />
        <BackendView v-show="route === 'backend'" />
      </main>
    </div>
  </el-config-provider>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import en from 'element-plus/es/locale/lang/en'
import BackendView from './views/BackendView.vue'
import LauncherView from './views/LauncherView.vue'
import SftpView from './views/SftpView.vue'
import ShellView from './views/ShellView.vue'
import { api } from './api'
import { locale, setLocale, t } from './i18n'

type RouteName = 'launcher' | 'sftp' | 'backend' | 'shell'

function initialRoute(): RouteName {
  if (location.pathname === '/sftp') return 'sftp'
  if (location.pathname === '/launcher-logs') return 'backend'
  if (location.pathname === '/shell') return 'shell'
  return 'launcher'
}

const route = ref<RouteName>(initialRoute())
const appVersion = ref('')
const iconPath = '/static/chemssh-icon.svg'
const elementLocale = computed(() => (locale.value === 'zh' ? zhCn : en))
const appTitle = computed(() => (appVersion.value ? `ChemSSH Launcher ${appVersion.value}` : 'ChemSSH Launcher'))
const navOptions = computed(() => [
  { label: t('app.launcher'), value: 'launcher' },
  { label: 'SFTP', value: 'sftp' },
  { label: t('app.backend'), value: 'backend' }
])
const subtitle = computed(() => {
  if (route.value === 'sftp') return t('app.sftpSubtitle')
  if (route.value === 'backend') return t('app.backendSubtitle')
  return t('app.launcherSubtitle')
})

watch(route, value => {
  const path = value === 'launcher' ? '/' : value === 'backend' ? '/launcher-logs' : `/${value}`
  if (location.pathname !== path) history.pushState(null, '', path)
})

window.addEventListener('popstate', () => {
  route.value = initialRoute()
})

onMounted(async () => {
  try {
    const data = await api<{ version: string }>('/api/version')
    appVersion.value = data.version.trim()
  } catch {
    appVersion.value = ''
  }
})
</script>
