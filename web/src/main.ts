import './assets/css/main.css'

import { createApp } from 'vue'
import { createPinia } from 'pinia'
import { VueQueryPlugin } from '@tanstack/vue-query'
import ui from '@nuxt/ui/vue-plugin'

import App from './App.vue'
import { router } from './router'

const app = createApp(App)

// Pinia before router: the router's auth guard calls useAuthStore()
// synchronously, so the pinia instance must already be installed.
app.use(createPinia())
app.use(router)
app.use(VueQueryPlugin)
app.use(ui)

app.mount('#app')
