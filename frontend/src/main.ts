import { createApp } from 'vue'
import {
  ElAlert,
  ElButton,
  ElCheckbox,
  ElConfigProvider,
  ElDropdown,
  ElDropdownItem,
  ElDropdownMenu,
  ElForm,
  ElFormItem,
  ElIcon,
  ElInput,
  ElInputNumber,
  ElOption,
  ElPopover,
  ElProgress,
  ElSegmented,
  ElSelect,
  ElTag,
  ElTooltip,
} from 'element-plus'
import 'element-plus/dist/index.css'
import './styles.css'
import './lumen.css'
import App from './App.vue'
import { initLocale } from './i18n'

const app = createApp(App)

const components = [
  ElAlert,
  ElButton,
  ElCheckbox,
  ElConfigProvider,
  ElDropdown,
  ElDropdownItem,
  ElDropdownMenu,
  ElForm,
  ElFormItem,
  ElIcon,
  ElInput,
  ElInputNumber,
  ElOption,
  ElPopover,
  ElProgress,
  ElSegmented,
  ElSelect,
  ElTag,
  ElTooltip,
]
for (const component of components) {
  app.component(component.name!, component)
}

async function bootstrap() {
  await initLocale()
  app.mount('#app')
}

void bootstrap()
