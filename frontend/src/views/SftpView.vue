<template>
  <section class="sftp-view">
    <div class="sftp-workspace">
      <section
        v-for="paneName in paneNames"
        :key="paneName"
        class="xftp-pane"
        :class="{ 'is-drop-active': panes[paneName].dropActive }"
        @dragenter="handleDragEnter(paneName, $event)"
        @dragover="handleDragOver(paneName, $event)"
        @dragleave="handleDragLeave(paneName, $event)"
        @drop="handleDrop(paneName, $event)"
      >
        <div class="xftp-tabs">
          <button
            v-for="tab in panes[paneName].tabs"
            :key="tab.id"
            class="xftp-tab"
            :class="{ active: tab.id === panes[paneName].active }"
            type="button"
            :title="tabTitle(tab)"
            @click="activateTab(paneName, tab.id)"
          >
            <el-icon><FolderOpened v-if="tab.kind === 'local'" /><Connection v-else /></el-icon>
            <span>{{ tabLabel(tab) }}</span>
            <span class="xftp-tab-close" role="button" tabindex="-1" @click.stop="closeTab(paneName, tab.id)">
              <el-icon><Close /></el-icon>
            </span>
          </button>

          <el-popover
            v-model:visible="panes[paneName].newMenuOpen"
            trigger="click"
            placement="bottom-start"
            popper-class="xftp-new-popover"
            :width="360"
          >
            <template #reference>
              <el-button class="xftp-add-tab" :icon="Plus" circle />
            </template>
            <div class="xftp-new-menu">
              <el-segmented v-model="panes[paneName].draft.kind" :options="newTabOptions" />
              <div v-if="panes[paneName].draft.kind === 'local'" class="xftp-new-form">
                <el-input v-model="panes[paneName].draft.localPath" placeholder="本地路径" @keydown.enter="openDraftTab(paneName)" />
                <el-button :icon="Select" type="primary" @click="openDraftTab(paneName)">打开本地</el-button>
              </div>
              <div v-else class="xftp-new-form">
                <el-select v-model="panes[paneName].draft.profileID" placeholder="选择已保存服务器" :teleported="false" @click.stop>
                  <el-option
                    v-for="profile in remoteProfiles"
                    :key="profile.id"
                    :label="`${profile.name || profile.ssh_host} - ${profile.ssh_user || '?'}@${profile.ssh_host || '?'}`"
                    :value="profile.id"
                  />
                </el-select>
                <el-input v-model="panes[paneName].draft.remotePath" placeholder="远程路径" @keydown.enter="openDraftTab(paneName)" />
                <el-button :icon="Connection" type="primary" @click="openDraftTab(paneName)">连接远程</el-button>
              </div>
            </div>
          </el-popover>
        </div>

        <div class="path-bar">
          <el-input
            class="path-input"
            :model-value="activeTab(paneName)?.path || ''"
            @update:model-value="setPath(paneName, String($event))"
            @keydown.enter="goPath(paneName)"
          />
        </div>

        <div class="file-toolbar">
          <el-tooltip content="刷新" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
            <el-button :icon="Refresh" circle @click="loadTab(paneName)" />
          </el-tooltip>
          <div class="toolbar-history-control">
            <el-tooltip content="返回" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
              <el-button :icon="Back" circle :disabled="!canGoBack(paneName)" @click="goBack(paneName)" />
            </el-tooltip>
            <el-dropdown trigger="click" :disabled="historyEntries(paneName).length === 0" @command="handleHistoryCommand(paneName, $event)">
              <button
                class="toolbar-history-menu-button"
                type="button"
                :disabled="historyEntries(paneName).length === 0"
                aria-label="历史记录"
              >
                <el-icon><CaretBottom /></el-icon>
              </button>
              <template #dropdown>
                <el-dropdown-menu>
                  <el-dropdown-item v-for="entry in historyEntries(paneName)" :key="entry.path" :command="entry.path">
                    <span class="toolbar-history-entry" :title="entry.path">{{ entry.label }}</span>
                  </el-dropdown-item>
                </el-dropdown-menu>
              </template>
            </el-dropdown>
          </div>
          <div class="toolbar-menu">
            <el-button :icon="Plus" circle aria-haspopup="menu" @click.prevent />
            <div class="toolbar-submenu" role="menu">
              <el-tooltip content="新建文件" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
                <button class="toolbar-submenu-button" type="button" role="menuitem" aria-label="新建文件" @click="createFile(paneName)">
                  <el-icon><DocumentAdd /></el-icon>
                </button>
              </el-tooltip>
              <el-tooltip content="新建文件夹" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
                <button class="toolbar-submenu-button" type="button" role="menuitem" aria-label="新建文件夹" @click="createFolder(paneName)">
                  <el-icon><FolderAdd /></el-icon>
                </button>
              </el-tooltip>
            </div>
          </div>
          <el-tooltip :content="activeTab(paneName)?.showHidden ? '隐藏点开头文件' : '显示点开头文件'" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
            <el-button
              :icon="activeTab(paneName)?.showHidden ? View : Hide"
              circle
              :type="activeTab(paneName)?.showHidden ? 'primary' : 'default'"
              @click="toggleHidden(paneName)"
            />
          </el-tooltip>
          <span class="muted">{{ paneLabel(paneName) }}</span>
          <div class="toolbar-spacer" />
          <div class="toolbar-menu">
            <el-button :icon="Upload" circle :disabled="activeTab(paneName)?.kind !== 'remote'" aria-haspopup="menu" @click.prevent />
            <div class="toolbar-submenu" role="menu">
              <el-tooltip content="上传文件" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
                <button class="toolbar-submenu-button" type="button" role="menuitem" :disabled="activeTab(paneName)?.kind !== 'remote'" @click="pickUpload(paneName, 'file')">
                  <el-icon><Upload /></el-icon>
                </button>
              </el-tooltip>
              <el-tooltip content="上传文件夹" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
                <button class="toolbar-submenu-button" type="button" role="menuitem" :disabled="activeTab(paneName)?.kind !== 'remote'" @click="pickUpload(paneName, 'folder')">
                  <el-icon><FolderOpened /></el-icon>
                </button>
              </el-tooltip>
            </div>
          </div>
          <el-tooltip content="下载远程文件" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
            <el-button :icon="Download" circle :disabled="!canDownload(paneName)" @click="downloadSelected(paneName)" />
          </el-tooltip>
          <el-tooltip content="重命名" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
            <el-button :icon="Edit" circle :disabled="selectedEntries(paneName).length !== 1" @click="renameSelected(paneName)" />
          </el-tooltip>
          <el-tooltip content="删除选中项" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
            <el-button :icon="Delete" circle type="danger" :disabled="selectedEntries(paneName).length === 0" @click="deleteSelected(paneName)" />
          </el-tooltip>
          <input :ref="el => setUploadInput(paneName, 'file', el)" class="hidden-input" type="file" multiple @change="handleUploadInput(paneName, $event)" />
          <input
            :ref="el => setUploadInput(paneName, 'folder', el)"
            class="hidden-input"
            type="file"
            multiple
            webkitdirectory
            directory
            @change="handleUploadInput(paneName, $event)"
          />
        </div>

          <div
            :ref="el => setTreeRef(paneName, el)"
            class="file-tree"
            :class="{ 'is-column-resizing': isColumnResizing(paneName) }"
            :style="columnStyle(paneName)"
          >
          <div class="file-list-header" role="row">
            <span role="columnheader" />
            <span class="file-header-cell" role="columnheader" :aria-sort="sortAria(paneName, 'name')">
              <button class="file-sort-button" type="button" :class="{ 'is-active': panes[paneName].sortKey === 'name' }" :title="sortTitle(paneName, 'name')" @click="toggleSort(paneName, 'name')">
                <span>名称</span>
                <span class="sort-icon-wrap">
                  <el-icon v-if="panes[paneName].sortKey === 'name'">
                    <CaretTop v-if="panes[paneName].sortDirection === 'asc'" />
                    <CaretBottom v-else />
                  </el-icon>
                </span>
              </button>
            </span>
            <button
              class="file-column-resizer file-column-resizer-name-size"
              type="button"
              role="separator"
              aria-label="调整名称和大小列宽"
              tabindex="0"
              @pointerdown="startColumnResize(paneName, 'name-size', $event)"
              @dblclick="resetColumnWidths(paneName)"
            />
            <span class="file-header-cell file-header-size" role="columnheader" :aria-sort="sortAria(paneName, 'size')">
              <button class="file-sort-button" type="button" :class="{ 'is-active': panes[paneName].sortKey === 'size' }" :title="sortTitle(paneName, 'size')" @click="toggleSort(paneName, 'size')">
                <span>大小</span>
                <span class="sort-icon-wrap">
                  <el-icon v-if="panes[paneName].sortKey === 'size'">
                    <CaretTop v-if="panes[paneName].sortDirection === 'asc'" />
                    <CaretBottom v-else />
                  </el-icon>
                </span>
              </button>
            </span>
            <button
              class="file-column-resizer file-column-resizer-size-time"
              type="button"
              role="separator"
              aria-label="调整大小和修改时间列宽"
              tabindex="0"
              @pointerdown="startColumnResize(paneName, 'size-time', $event)"
              @dblclick="resetColumnWidths(paneName)"
            />
            <span class="file-header-cell" role="columnheader" :aria-sort="sortAria(paneName, 'mtime')">
              <button class="file-sort-button" type="button" :class="{ 'is-active': panes[paneName].sortKey === 'mtime' }" :title="sortTitle(paneName, 'mtime')" @click="toggleSort(paneName, 'mtime')">
                <span>修改时间</span>
                <span class="sort-icon-wrap">
                  <el-icon v-if="panes[paneName].sortKey === 'mtime'">
                    <CaretTop v-if="panes[paneName].sortDirection === 'asc'" />
                    <CaretBottom v-else />
                  </el-icon>
                </span>
              </button>
            </span>
          </div>
          <div
            :ref="el => setBodyRef(paneName, el)"
            class="file-list-body"
            role="rowgroup"
            @scroll="handleBodyScroll(paneName)"
            @mousedown.left="beginBlankSelect(paneName, $event)"
          >
            <div v-if="activeTab(paneName)?.loading" class="empty-state compact">正在读取目录...</div>
            <div v-else-if="!activeTab(paneName)" class="empty-state compact">没有打开的标签页。</div>
            <template v-else>
              <div
                v-if="parentDirectoryEntry(paneName)"
                class="file-row file-parent-row is-directory"
                role="row"
                tabindex="0"
                draggable="false"
                @mousedown.left.stop
                @click.prevent
                @dblclick="openParentDirectory(paneName)"
                @keydown.enter.prevent="openParentDirectory(paneName)"
              >
                <span class="file-cell file-icon-cell">
                  <img
                    v-if="!parentDirectoryIconFailed(paneName)"
                    class="file-system-icon"
                    :src="parentDirectoryIconURL(paneName)"
                    alt=""
                    draggable="false"
                    @load="markParentDirectoryIconLoaded(paneName)"
                    @error="markParentDirectoryIconFailed(paneName)"
                  />
                  <el-icon v-else class="file-kind-icon"><Folder /></el-icon>
                </span>
                <span class="file-cell file-name-cell" :title="parentDirectoryEntry(paneName)?.path || '..'">..</span>
                <span class="file-column-divider file-column-divider-name-size" aria-hidden="true" />
                <span class="file-cell file-size-cell" />
                <span class="file-column-divider file-column-divider-size-time" aria-hidden="true" />
                <span class="file-cell file-time-cell" />
              </div>
              <div v-if="visibleEntries(paneName).length === 0" class="empty-state compact">目录为空。</div>
              <div class="file-list-virtual-spacer" :style="{ height: `${topSpacerHeight(paneName)}px` }" aria-hidden="true" />
              <div
                v-for="{ entry, index } in virtualRows(paneName)"
                :key="entry.path"
                class="file-row"
                :class="{
                  'is-directory': entry.is_dir,
                  'is-selected': isSelected(paneName, entry),
                  'is-drag-range': isInDragRange(paneName, index, entry),
                  'is-drag-export': isDragSource(paneName, entry),
                  'is-move-drop-target': isFolderDropTarget(paneName, entry)
                }"
                role="row"
                tabindex="0"
                draggable="true"
                @mousedown.left="beginSelect(paneName, index, entry, $event)"
                @click.prevent
                @contextmenu.prevent="openFileContextMenu(paneName, index, entry, $event)"
                @dblclick="openEntry(paneName, entry)"
                @mouseenter="extendSelection(paneName, index)"
                @dragstart="startFileDrag(paneName, index, entry, $event)"
                @dragenter="handleFolderDragOver(paneName, entry, $event)"
                @dragover="handleFolderDragOver(paneName, entry, $event)"
                @dragleave="handleFolderDragLeave(paneName, entry, $event)"
                @drop="handleFolderDrop(paneName, entry, $event)"
                @dragend="finishFileDrag"
                @keydown.enter.prevent="openEntry(paneName, entry)"
              >
                <span class="file-cell file-icon-cell">
                  <img
                    v-if="!systemIconFailed(entry)"
                    class="file-system-icon"
                    :src="fileIconURL(entry)"
                    alt=""
                    draggable="false"
                    @load="markSystemIconLoaded(entry)"
                    @error="markSystemIconFailed(entry)"
                  />
                  <el-icon v-else class="file-kind-icon"><Folder v-if="entry.is_dir" /><Document v-else /></el-icon>
                </span>
                <span class="file-cell file-name-cell" :title="entry.path">{{ entry.name }}</span>
                <span class="file-column-divider file-column-divider-name-size" aria-hidden="true" />
                <span class="file-cell file-size-cell">{{ entry.is_dir ? '' : formatBytes(entry.size) }}</span>
                <span class="file-column-divider file-column-divider-size-time" aria-hidden="true" />
                <span class="file-cell file-time-cell">{{ formatDate(entry.mod_time) }}</span>
              </div>
              <div class="file-list-virtual-spacer" :style="{ height: `${bottomSpacerHeight(paneName)}px` }" aria-hidden="true" />
            </template>
          </div>
          <div
            v-if="panes[paneName].scrollState.showVertical"
            class="file-floating-scrollbar is-vertical"
            aria-hidden="true"
            @pointerdown="handleScrollbarTrackPointerDown(paneName, 'vertical', $event)"
          >
            <div
              class="file-floating-scrollbar-thumb"
              :style="{
                height: `${panes[paneName].scrollState.verticalThumbSize}px`,
                transform: `translateY(${panes[paneName].scrollState.verticalThumbOffset}px)`
              }"
              @pointerdown.stop="startScrollbarThumbDrag(paneName, 'vertical', $event)"
            />
          </div>
          <div
            v-if="panes[paneName].scrollState.showHorizontal"
            class="file-floating-scrollbar is-horizontal"
            aria-hidden="true"
            @pointerdown="handleScrollbarTrackPointerDown(paneName, 'horizontal', $event)"
          >
            <div
              class="file-floating-scrollbar-thumb"
              :style="{
                width: `${panes[paneName].scrollState.horizontalThumbSize}px`,
                transform: `translateX(${panes[paneName].scrollState.horizontalThumbOffset}px)`
              }"
              @pointerdown.stop="startScrollbarThumbDrag(paneName, 'horizontal', $event)"
            />
          </div>
        </div>

        <div
          v-if="isCurrentDirectoryDropActive(paneName)"
          class="canvas-file-drop-actions sftp-current-drop-actions"
          aria-live="polite"
          @dragleave.stop="handleCurrentDirectoryDropLeave(paneName, $event)"
        >
          <div
            class="canvas-file-drop-overlay"
            @dragenter.stop.prevent="handleCurrentDirectoryDropOver(paneName, $event)"
            @dragover.stop.prevent="handleCurrentDirectoryDropOver(paneName, $event)"
            @drop.stop.prevent="handleCurrentDirectoryDrop(paneName, $event)"
          >
            <el-icon><Upload /></el-icon>
            <span>松开以传输到当前目录</span>
          </div>
        </div>

        <div v-else-if="panes[paneName].dropActive" class="canvas-file-drop-overlay is-upload-overlay">
          <el-icon><Upload /></el-icon>
          <span>{{ dropHint(paneName) }}</span>
        </div>
      </section>
    </div>

    <section class="transfer-panel">
      <div class="logs-title">
        <div>
          <h2>{{ t('sftp.transferPanel') }}</h2>
          <p>{{ transferPanelMode === 'transfers' ? activeTransferSummary : t('sftp.rawLogs') }}</p>
        </div>
        <div class="header-actions">
          <el-segmented v-model="transferPanelMode" :options="transferPanelOptions" />
          <el-tooltip :content="t('sftp.refreshBoth')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
            <el-button :icon="Refresh" circle @click="refreshAll" />
          </el-tooltip>
          <el-tooltip :content="t('sftp.clearLogs')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
            <el-button :icon="Delete" circle @click="clearTransferPanel" />
          </el-tooltip>
        </div>
      </div>
      <div v-if="transferPanelMode === 'transfers'" class="transfer-list-shell">
        <div
          :ref="setTransferListRef"
          class="transfer-list"
          @scroll="updateTransferScrollbars"
        >
          <div class="transfer-row transfer-row-header" role="row">
            <span>{{ t('sftp.name') }}</span>
            <span>{{ t('sftp.status') }}</span>
            <span>{{ t('sftp.progress') }}</span>
            <span>{{ t('sftp.size') }}</span>
            <span>{{ t('sftp.sourcePath') }}</span>
            <span>&lt;-&gt;</span>
            <span>{{ t('sftp.targetPath') }}</span>
            <span>{{ t('sftp.speed') }}</span>
            <span>{{ t('sftp.eta') }}</span>
            <span>{{ t('sftp.elapsed') }}</span>
          </div>
          <div v-if="transferItems.length === 0" class="transfer-empty">{{ t('sftp.emptyTransfers') }}</div>
          <div
            v-for="item in visibleTransferItems"
            :key="item.id"
            class="transfer-row"
            :class="[`is-${item.status}`, { 'is-group': item.isGroup, 'is-child': item.parentID, 'is-paused': item.paused, 'is-selected': isTransferSelected(item) }]"
            role="row"
            @mousedown.left="selectTransferRow(item, $event)"
            @contextmenu.prevent="openTransferContextMenu(item, $event)"
          >
            <div class="transfer-name-cell">
              <button
                v-if="item.isGroup"
                class="transfer-toggle"
                type="button"
                :aria-label="item.expanded ? t('sftp.collapseFolderTransfer') : t('sftp.expandFolderTransfer')"
                @click.stop="toggleTransferGroup(item)"
              >
                <el-icon><ArrowDown v-if="item.expanded" /><ArrowRight v-else /></el-icon>
              </button>
              <span v-else class="transfer-toggle-placeholder" />
              <strong :title="item.name">{{ item.name }}</strong>
              <span v-if="item.isGroup" class="transfer-group-summary">{{ transferGroupSummary(item) }}</span>
            </div>
            <span class="transfer-status" :title="item.error || transferStatusLabel(item)">
              <i
                class="status-dot"
                :class="item.paused ? 'is-paused' : `is-${item.status}`"
                aria-hidden="true"
              />
              {{ transferStatusLabel(item) }}
            </span>
            <div class="transfer-progress-cell">
              <el-progress :percentage="transferPercent(item)" :show-text="false" :status="item.status === 'error' ? 'exception' : item.status === 'done' ? 'success' : undefined" />
              <span>{{ transferPercent(item) }}%</span>
            </div>
            <span>{{ formatBytes(item.loaded) }} / {{ item.total ? formatBytes(item.total) : t('sftp.unknownSize') }}</span>
            <span :title="`${transferEndpointLabel(item.sourceName)}: ${item.sourcePath}`">{{ transferEndpointLabel(item.sourceName) }}: {{ item.sourcePath }}</span>
            <span class="transfer-arrow-cell">→</span>
            <span :title="`${transferEndpointLabel(item.targetName)}: ${item.targetPath}`">{{ transferEndpointLabel(item.targetName) }}: {{ item.targetPath }}</span>
            <span>{{ transferSpeed(item) }}</span>
            <span>{{ transferEta(item) }}</span>
            <span>{{ transferElapsed(item) }}</span>
          </div>
        </div>
        <div
          v-if="transferScrollState.showVertical"
          class="file-floating-scrollbar transfer-floating-scrollbar is-vertical"
          aria-hidden="true"
          @pointerdown="handleTransferScrollbarTrackPointerDown('vertical', $event)"
        >
          <div
            class="file-floating-scrollbar-thumb"
            :style="{ height: `${transferScrollState.verticalThumbSize}px`, transform: `translateY(${transferScrollState.verticalThumbOffset}px)` }"
            @pointerdown.stop="startTransferScrollbarThumbDrag('vertical', $event)"
          />
        </div>
        <div
          v-if="transferScrollState.showHorizontal"
          class="file-floating-scrollbar transfer-floating-scrollbar is-horizontal"
          aria-hidden="true"
          @pointerdown="handleTransferScrollbarTrackPointerDown('horizontal', $event)"
        >
          <div
            class="file-floating-scrollbar-thumb"
            :style="{ width: `${transferScrollState.horizontalThumbSize}px`, transform: `translateX(${transferScrollState.horizontalThumbOffset}px)` }"
            @pointerdown.stop="startTransferScrollbarThumbDrag('horizontal', $event)"
          />
        </div>
      </div>
      <pre v-else ref="transferLogRef" @scroll="transferLogScroller.onScroll">{{ transferLogs.join('\n') }}</pre>
    </section>

    <Teleport to="body">
      <div v-if="contextMenu.visible" class="sftp-context-menu" :style="{ left: `${contextMenu.x}px`, top: `${contextMenu.y}px` }" role="menu">
        <button type="button" role="menuitem" :disabled="!contextMenu.entry" @click="runContextAction('open')">打开</button>
        <button type="button" role="menuitem" :disabled="!contextMenu.entry || contextMenu.entry.is_dir" @click="runContextAction('openText')">用记事本编辑</button>
        <button type="button" role="menuitem" :disabled="!contextMenu.entry" @click="runContextAction('copyPath')">复制路径</button>
        <span class="sftp-context-separator" />
        <button type="button" role="menuitem" :disabled="!contextMenu.entry" @click="runContextAction('rename')">重命名</button>
        <button type="button" role="menuitem" class="is-danger" :disabled="!contextMenu.entry" @click="runContextAction('delete')">删除</button>
      </div>
    </Teleport>

    <Teleport to="body">
      <div v-if="transferContextMenu.visible" class="sftp-context-menu" :style="{ left: `${transferContextMenu.x}px`, top: `${transferContextMenu.y}px` }" role="menu">
        <button type="button" role="menuitem" :disabled="!canPauseTransfer(transferContextMenu.item)" @click="runTransferContextAction('pause')">{{ t('sftp.pause') }}</button>
        <button type="button" role="menuitem" :disabled="!canResumeTransfer(transferContextMenu.item)" @click="runTransferContextAction('resume')">{{ t('sftp.resume') }}</button>
        <span class="sftp-context-separator" />
        <button type="button" role="menuitem" class="is-danger" :disabled="!canCancelTransfer(transferContextMenu.item)" @click="runTransferContextAction('cancel')">{{ t('sftp.cancel') }}</button>
      </div>
    </Teleport>

    <Teleport to="body">
      <div v-if="conflictDialog.visible" class="upload-conflict-backdrop">
        <div class="upload-conflict-dialog" role="dialog" aria-modal="true">
          <div class="upload-conflict-title">目标中已存在同名项目</div>
          <p>“{{ conflictDialog.name }}” 已存在。请选择如何处理。</p>
          <label class="upload-conflict-apply">
            <input v-model="conflictDialog.applyAll" type="checkbox" />
            <span>对后续冲突应用相同操作</span>
          </label>
          <div class="upload-conflict-actions">
            <el-button type="primary" @click="chooseConflict('overwrite')">覆盖</el-button>
            <el-button @click="chooseConflict('skip')">跳过</el-button>
            <el-button @click="chooseConflict('suffix')">加后缀</el-button>
            <el-button @click="chooseConflict('cancel')">取消</el-button>
          </div>
        </div>
      </div>
    </Teleport>
  </section>
</template>

<script setup lang="ts">
import { computed, markRaw, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  ArrowDown,
  ArrowRight,
  Back,
  CaretBottom,
  CaretTop,
  Close,
  Connection,
  Delete,
  Document,
  DocumentAdd,
  Download,
  Edit,
  Folder,
  FolderAdd,
  FolderOpened,
  Hide,
  Plus,
  Refresh,
  Select,
  Upload,
  View
} from '@element-plus/icons-vue'
import { api, APIError, baseName, formatBytes, formatDate, joinPath, launcherToken, launcherTokenHeader, parentPath, postJSON, type FileEntry, type HostKeyInfo, type Profile } from '../api'
import { createAutoScroll } from '../autoScroll'
import {
  collectDropUploadEntries,
  filesToUploadEntries,
  isSafeUploadRelativePath,
  normalizeUploadRelativePath,
  normalizeUploadEntries,
  setUploadDropEffect,
  type UploadConflictAction,
  type UploadConflictResolution,
  type UploadEntry
} from '../uploadEntries'
import { locale, t } from '../i18n'

type PaneName = 'left' | 'right'
type TabKind = 'local' | 'remote'
type ColumnResizeTarget = 'name-size' | 'size-time'
type ScrollbarAxis = 'vertical' | 'horizontal'
type TransferPanelMode = 'transfers' | 'logs'
type TransferStatus = 'running' | 'done' | 'error'
type SortKey = 'name' | 'size' | 'mtime'
type SortDirection = 'asc' | 'desc'

interface FileTab {
  id: string
  kind: TabKind
  title: string
  path: string
  loadedPath: string
  entries: FileEntry[]
  selected: FileEntry[]
  loading: boolean
  showHidden: boolean
  anchorPath: string
  history: string[]
  session?: string
  profileID?: string
  profileName?: string
}

interface PaneState {
  tabs: FileTab[]
  active: string
  newMenuOpen: boolean
  dropActive: boolean
  treeRef: HTMLElement | null
  bodyRef: HTMLElement | null
  bodyScrollTop: number
  bodyViewportHeight: number
  sizeColumnWidth: number
  timeColumnWidth: number
  sortKey: SortKey
  sortDirection: SortDirection
  scrollState: ScrollState
  draft: {
    kind: TabKind
    localPath: string
    remotePath: string
    profileID: string
  }
}

interface ScrollState {
  showVertical: boolean
  showHorizontal: boolean
  verticalThumbSize: number
  verticalThumbOffset: number
  horizontalThumbSize: number
  horizontalThumbOffset: number
}

interface VirtualFileRow {
  entry: FileEntry
  index: number
}

interface DragPayload {
  pane: PaneName
  tabID: string
  paths: string[]
}

interface ConflictDialogState {
  visible: boolean
  name: string
  applyAll: boolean
  resolve: ((resolution: UploadConflictResolution) => void) | null
}

interface ContextMenuState {
  visible: boolean
  x: number
  y: number
  pane: PaneName | null
  entry: FileEntry | null
}

interface TransferContextMenuState {
  visible: boolean
  x: number
  y: number
  item: TransferItem | null
}

interface RelativeTransfer<T> {
  payload: T
  relativePath: string
  displayPath: string
  rootName: string
  isDir: boolean
}

interface TransferItem {
  id: string
  name: string
  kind: 'file' | 'folder'
  sourceName: string
  targetName: string
  sourcePath: string
  targetPath: string
  loaded: number
  total: number
  startedAt: number
  endedAt: number | null
  status: TransferStatus
  error: string
  paused?: boolean
  cancelled?: boolean
  parentID?: string
  isGroup?: boolean
  expanded?: boolean
}

interface TransferProgress {
  id: string
  loaded: number
  total: number
  done: boolean
  paused: boolean
  cancelled: boolean
  error: string
}

interface OpenSyncEvent {
  seq: number
  session: string
  remote_path: string
  local_path: string
  status: 'done' | 'error'
  error?: string
}

interface ScrollbarDragState {
  pane: PaneName
  axis: ScrollbarAxis
  pointerId: number
  startClient: number
  startScroll: number
  maxScroll: number
  maxThumbOffset: number
}

interface TransferScrollbarDragState {
  axis: ScrollbarAxis
  pointerId: number
  startClient: number
  startScroll: number
  maxScroll: number
  maxThumbOffset: number
}

const paneNames = ['left', 'right'] as const
const appDragType = 'application/x-chemssh-launcher-file-transfer'
const safeNamePattern = /^[^/\\]+$/
const LONG_PRESS_MS = 430
const SELECT_DRAG_THRESHOLD = 4
const DEFAULT_SIZE_COLUMN_WIDTH = 88
const DEFAULT_TIME_COLUMN_WIDTH = 152
const MIN_NAME_COLUMN_WIDTH = 160
const MIN_SIZE_COLUMN_WIDTH = 64
const MAX_SIZE_COLUMN_WIDTH = 168
const MIN_TIME_COLUMN_WIDTH = 112
const MAX_TIME_COLUMN_WIDTH = 260
const FILE_ICON_COLUMN_WIDTH = 34
const FILE_COLUMN_RESIZER_WIDTH = 10
const FLOATING_SCROLLBAR_MIN_THUMB = 36
const FILE_ROW_SLOT_HEIGHT = 40
const FILE_BODY_VERTICAL_PADDING = 4
const VIRTUAL_ROW_OVERSCAN = 10
const TRANSFER_PROGRESS_POLL_MS = 80
const DIRECTORY_HISTORY_LIMIT = 20
const newTabOptions = computed(() => [
  { label: t('sftp.local'), value: 'local' },
  { label: t('sftp.remote'), value: 'remote' }
])
const transferPanelOptions = computed(() => [
  { label: t('sftp.transfer'), value: 'transfers' },
  { label: t('sftp.logs'), value: 'logs' }
])
const profiles = ref<Profile[]>([])
const remoteProfiles = computed(() => profiles.value.filter(profile => profile.kind !== 'local'))
const home = ref('.')
const transferLogs = ref<string[]>([])
const transferItems = ref<TransferItem[]>([])
const transferPanelMode = ref<TransferPanelMode>('transfers')
const transferListRef = ref<HTMLElement | null>(null)
const transferLogRef = ref<HTMLElement | null>(null)
const transferLogScroller = createAutoScroll(() => transferLogRef.value)
const selectedTransferIDs = ref(new Set<string>())
const transferAnchorID = ref('')
const failedSystemIconKeys = ref(new Set<string>())
const transferScrollState = ref<ScrollState>({
  showVertical: false,
  showHorizontal: false,
  verticalThumbSize: FLOATING_SCROLLBAR_MIN_THUMB,
  verticalThumbOffset: 0,
  horizontalThumbSize: FLOATING_SCROLLBAR_MIN_THUMB,
  horizontalThumbOffset: 0
})
type UploadKind = 'file' | 'folder'

const uploadInputs: Record<PaneName, Partial<Record<UploadKind, HTMLInputElement>>> = {
  left: {},
  right: {}
}
const dragging = ref<DragPayload | null>(null)
const folderDropTarget = ref<{ pane: PaneName; path: string } | null>(null)
const activeColumnResize = ref<{ pane: PaneName; target: ColumnResizeTarget } | null>(null)
const rangeDragPane = ref<PaneName | null>(null)
const rangeDragAnchor = ref<number | null>(null)
const rangeDragOver = ref<number | null>(null)
const blankDragPane = ref<PaneName | null>(null)
const blankDragPathSet = ref(new Set<string>())
const exportDragPathSet = ref(new Set<string>())
const exportDragArmed = ref(false)
const exportDragActive = ref(false)
const conflictDialog = ref<ConflictDialogState>({
  visible: false,
  name: '',
  applyAll: false,
  resolve: null
})
const contextMenu = ref<ContextMenuState>({
  visible: false,
  x: 0,
  y: 0,
  pane: null,
  entry: null
})
const transferContextMenu = ref<TransferContextMenuState>({
  visible: false,
  x: 0,
  y: 0,
  item: null
})

const panes = reactive<Record<PaneName, PaneState>>({
  left: makePane(),
  right: makePane()
})

let columnResizeStartX = 0
let columnResizeStartSize = 0
let columnResizeStartTime = 0
let previousBodyCursor = ''
let previousBodyUserSelect = ''
let pressPane: PaneName | null = null
let pressIndex: number | null = null
let pressStartX = 0
let pressStartY = 0
let longPressTimer: number | null = null
let blankDragStartX = 0
let blankDragStartY = 0
let blankDragCurrentX = 0
let blankDragCurrentY = 0
let lastOpenSyncSeq = 0
let openSyncPollTimer: number | undefined
let openSyncPolling = false
let scrollResizeObserver: ResizeObserver | null = null
let activeScrollbarDrag: ScrollbarDragState | null = null
let activeTransferScrollbarDrag: TransferScrollbarDragState | null = null

const activeTransferSummary = computed(() => {
  const running = transferItems.value.filter(item => item.status === 'running').length
  if (running > 0) return t('sftp.runningSummary', { count: running })
  const done = transferItems.value.filter(item => item.status === 'done').length
  const failed = transferItems.value.filter(item => item.status === 'error').length
  if (done || failed) return t('sftp.doneFailedSummary', { done, failed })
  return t('sftp.waitingTransfers')
})

const visibleTransferItems = computed(() => {
  const topLevel: TransferItem[] = []
  const childrenByParent = new Map<string, TransferItem[]>()
  for (const item of transferItems.value) {
    if (!item.parentID) {
      topLevel.push(item)
      continue
    }
    const children = childrenByParent.get(item.parentID)
    if (children) children.push(item)
    else childrenByParent.set(item.parentID, [item])
  }
  const visible: TransferItem[] = []
  for (const item of topLevel) {
    visible.push(item)
    if (item.isGroup && item.expanded) {
      visible.push(...(childrenByParent.get(item.id) || []))
    }
  }
  return visible
})

const visibleEntriesByPane = computed<Record<PaneName, FileEntry[]>>(() => ({
  left: visibleEntriesForPane('left'),
  right: visibleEntriesForPane('right')
}))

const fileNameCollator = computed(() => new Intl.Collator(locale.value === 'zh' ? 'zh-CN' : 'en-US', {
  numeric: true,
  sensitivity: 'base'
}))

function makePane(): PaneState {
  return {
    tabs: [],
    active: '',
    newMenuOpen: false,
    dropActive: false,
    treeRef: null,
    bodyRef: null,
    bodyScrollTop: 0,
    bodyViewportHeight: 0,
    sizeColumnWidth: DEFAULT_SIZE_COLUMN_WIDTH,
    timeColumnWidth: DEFAULT_TIME_COLUMN_WIDTH,
    sortKey: 'name',
    sortDirection: 'asc',
    scrollState: {
      showVertical: false,
      showHorizontal: false,
      verticalThumbSize: FLOATING_SCROLLBAR_MIN_THUMB,
      verticalThumbOffset: 0,
      horizontalThumbSize: FLOATING_SCROLLBAR_MIN_THUMB,
      horizontalThumbOffset: 0
    },
    draft: { kind: 'local', localPath: '.', remotePath: '.', profileID: '' }
  }
}

function newID() {
  return crypto.randomUUID ? crypto.randomUUID() : `${Date.now()}-${Math.random().toString(16).slice(2)}`
}

function activeTab(paneName: PaneName) {
  const pane = panes[paneName]
  return pane.tabs.find(tab => tab.id === pane.active) || pane.tabs[0]
}

function tabByID(paneName: PaneName, tabID: string) {
  return panes[paneName].tabs.find(tab => tab.id === tabID)
}

function activateTab(paneName: PaneName, tabID: string) {
  panes[paneName].active = tabID
  nextTick(() => updateVirtualMetrics(paneName))
}

function localTab(path: string): FileTab {
  return {
    id: newID(),
    kind: 'local',
    title: `${t('sftp.local')}:${baseName(path) || path}`,
    path,
    loadedPath: path,
    entries: [],
    selected: [],
    loading: false,
    showHidden: false,
    anchorPath: '',
    history: []
  }
}

function pendingRemoteTab(profile: Profile, path: string): FileTab {
  const initialPath = path || '.'
  return {
    id: newID(),
    kind: 'remote',
    title: `${profile.name || t('sftp.remote')}:${baseName(initialPath) || initialPath}`,
    path: initialPath,
    loadedPath: initialPath,
    entries: [],
    selected: [],
    loading: true,
    showHidden: false,
    anchorPath: '',
    history: [],
    profileID: profile.id,
    profileName: profile.name || profile.ssh_host
  }
}

function tabTitle(tab: FileTab) {
  return tab.kind === 'remote' ? `${tab.profileName || t('sftp.remote')} ${tab.path}` : tab.path
}

function tabLabel(tab: FileTab) {
  const name = baseName(tab.path) || tab.path
  if (tab.kind === 'local') return `${t('sftp.local')}:${name}`
  return `${tab.profileName || t('sftp.remote')}:${name}`
}

function paneLabel(paneName: PaneName) {
  const tab = activeTab(paneName)
  if (!tab) return ''
  const count = tab.selected.length
  const suffix = count > 0 ? `，已选 ${count} 项` : ''
  return `${tab.kind === 'remote' ? `SFTP: ${tab.profileName}` : '本地文件'}${suffix}`
}

function transferEndpointName(tab: FileTab | null | undefined) {
  if (!tab) return t('sftp.unknown')
  return tab.kind === 'remote' ? (tab.profileName || tab.profileID || t('sftp.remoteServer')) : t('sftp.local')
}

function transferEndpointLabel(name: string) {
  if (name === '本地' || name === 'Local') return t('sftp.local')
  if (name === '远程服务器' || name === 'Remote server') return t('sftp.remoteServer')
  if (name === '未知' || name === 'Unknown') return t('sftp.unknown')
  return name
}

function setUploadInput(paneName: PaneName, kind: UploadKind, el: unknown) {
  if (el instanceof HTMLInputElement) uploadInputs[paneName][kind] = el
}

function setTreeRef(paneName: PaneName, el: unknown) {
  const nextEl = el instanceof HTMLElement ? el : null
  if (panes[paneName].treeRef === nextEl) return
  panes[paneName].treeRef = nextEl
  observeScrollElement(nextEl)
}

function setBodyRef(paneName: PaneName, el: unknown) {
  const nextEl = el instanceof HTMLElement ? el : null
  if (panes[paneName].bodyRef === nextEl) return
  panes[paneName].bodyRef = nextEl
  observeScrollElement(nextEl)
  nextTick(() => updateVirtualMetrics(paneName))
}

function setTransferListRef(el: unknown) {
  const nextEl = el instanceof HTMLElement ? el : null
  if (transferListRef.value === nextEl) return
  transferListRef.value = nextEl
  observeScrollElement(nextEl)
  nextTick(updateTransferScrollbars)
}

function observeScrollElement(el: HTMLElement | null) {
  if (!el || !scrollResizeObserver) return
  scrollResizeObserver.observe(el)
}

function log(message: string) {
  transferLogs.value.push(`${new Date().toLocaleTimeString()}  ${message}`)
  if (transferLogs.value.length > 200) transferLogs.value = transferLogs.value.slice(-200)
  nextTick(() => transferLogScroller.scrollToBottom())
}

function createTransferItem(input: Pick<TransferItem, 'name' | 'kind' | 'sourceName' | 'targetName' | 'sourcePath' | 'targetPath' | 'total'> & Partial<Pick<TransferItem, 'parentID' | 'isGroup' | 'expanded'>>) {
  const item: TransferItem = {
    id: newID(),
    ...input,
    loaded: 0,
    startedAt: Date.now(),
    endedAt: null,
    status: 'running',
    error: '',
    paused: false,
    cancelled: false
  }
  transferItems.value.unshift(item)
  if (transferItems.value.length > 300) transferItems.value = transferItems.value.slice(0, 300)
  transferPanelMode.value = 'transfers'
  nextTick(updateTransferScrollbars)
  return transferItems.value.find(current => current.id === item.id) || item
}

function updateTransferItem(item: TransferItem, loaded: number, total = item.total) {
  const target = currentTransferItem(item)
  target.loaded = Math.max(0, loaded)
  target.total = Math.max(total || 0, target.loaded)
  if (target.parentID) syncTransferGroup(target.parentID)
  touchTransferItems()
  nextTick(updateTransferScrollbars)
}

function finishTransferItem(item: TransferItem, status: TransferStatus, error = '') {
  const target = currentTransferItem(item)
  target.status = status
  target.error = error
  target.endedAt = Date.now()
  target.paused = false
  if (status === 'error' && /(cancel|取消)/i.test(error)) target.cancelled = true
  if (status === 'done' && target.total > 0) target.loaded = target.total
  if (target.parentID) syncTransferGroup(target.parentID)
  touchTransferItems()
  nextTick(updateTransferScrollbars)
}

function currentTransferItem(item: TransferItem) {
  return transferItems.value.find(current => current.id === item.id) || item
}

function touchTransferItems() {
  transferItems.value = [...transferItems.value]
}

function syncTransferGroup(groupID: string) {
  const group = transferItems.value.find(item => item.id === groupID)
  if (!group) return
  const children = transferItems.value.filter(item => item.parentID === groupID)
  if (children.length === 0) return
  group.total = children.reduce((sum, item) => sum + item.total, 0)
  group.loaded = children.reduce((sum, item) => sum + item.loaded, 0)
  group.status = children.some(item => item.status === 'running') ? 'running' : children.some(item => item.status === 'error') ? 'error' : 'done'
  group.paused = group.status === 'running' && children.some(item => item.paused)
  group.cancelled = children.every(item => item.cancelled)
  group.error = children.find(item => item.error)?.error || ''
  group.endedAt = group.status === 'running' ? null : Math.max(...children.map(item => item.endedAt || item.startedAt))
}

function toggleTransferGroup(item: TransferItem) {
  if (!item.isGroup) return
  item.expanded = !item.expanded
  nextTick(updateTransferScrollbars)
}

function transferGroupSummary(item: TransferItem) {
  if (!item.isGroup) return ''
  const children = transferItems.value.filter(child => child.parentID === item.id)
  if (children.length === 0) return ''
  const done = children.filter(child => child.status === 'done').length
  const failed = children.filter(child => child.status === 'error').length
  if (failed) return t('sftp.folderSummaryFailed', { done, total: children.length, failed })
  return t('sftp.folderSummary', { done, total: children.length })
}

function isTransferSelected(item: TransferItem) {
  return selectedTransferIDs.value.has(item.id)
}

function selectTransferRow(item: TransferItem, event: MouseEvent) {
  const visible = visibleTransferItems.value
  const currentIndex = visible.findIndex(entry => entry.id === item.id)
  if (event.shiftKey && transferAnchorID.value) {
    const anchorIndex = visible.findIndex(entry => entry.id === transferAnchorID.value)
    const start = anchorIndex >= 0 ? anchorIndex : currentIndex
    const from = Math.min(start, currentIndex)
    const to = Math.max(start, currentIndex)
    selectedTransferIDs.value = new Set(visible.slice(from, to + 1).map(entry => entry.id))
    return
  }
  if (event.ctrlKey || event.metaKey) {
    const next = new Set(selectedTransferIDs.value)
    if (next.has(item.id)) next.delete(item.id)
    else next.add(item.id)
    selectedTransferIDs.value = next
    transferAnchorID.value = item.id
    return
  }
  selectedTransferIDs.value = new Set([item.id])
  transferAnchorID.value = item.id
}

function selectedTransferTargets(item: TransferItem | null) {
  if (!item) return []
  if (!selectedTransferIDs.value.has(item.id)) return [item]
  return visibleTransferItems.value.filter(entry => selectedTransferIDs.value.has(entry.id))
}

function clearTransferPanel() {
  if (transferPanelMode.value === 'logs') {
    transferLogs.value = []
    nextTick(() => transferLogScroller.scrollToBottom(true))
    return
  }
  transferItems.value = transferItems.value.filter(item => item.status === 'running')
  selectedTransferIDs.value = new Set([...selectedTransferIDs.value].filter(id => transferItems.value.some(item => item.id === id)))
}

function runningTransferCount() {
  return transferItems.value.filter(item => item.status === 'running' && !item.isGroup).length
}

const runningCount = computed(() => runningTransferCount())

watch(runningCount, count => {
  postJSON('/api/transfer-count', { count }).catch(() => undefined)
})

watch(transferPanelMode, mode => {
  if (mode === 'logs') nextTick(() => transferLogScroller.scrollToBottom(true))
  else nextTick(updateTransferScrollbars)
})

function handleBeforeUnload(event: BeforeUnloadEvent) {
  const running = runningTransferCount()
  if (running <= 0) return
  const message = t('sftp.closeDuringTransfer', { count: running })
  event.preventDefault()
  event.returnValue = message
  return message
}

function transferPercent(item: TransferItem) {
  if (item.status === 'done') return 100
  if (!item.total) return item.loaded > 0 ? 100 : 0
  return Math.min(99, Math.round((item.loaded / item.total) * 100))
}

function transferStatusLabel(item: TransferItem) {
  if (item.status === 'done') return t('sftp.statusDone')
  if (item.cancelled) return t('sftp.statusCancelled')
  if (item.status === 'error') return t('sftp.statusError')
  if (item.paused) return t('sftp.statusPaused')
  return t('sftp.statusRunning')
}

function transferSpeed(item: TransferItem) {
  if (item.paused) return t('sftp.statusPaused')
  const elapsed = Math.max(0.001, ((item.endedAt || Date.now()) - item.startedAt) / 1000)
  if (item.loaded <= 0) return t('sftp.waitingData')
  return `${formatBytes(item.loaded / elapsed)}/s`
}

function transferElapsed(item: TransferItem) {
  return formatDuration(((item.endedAt || Date.now()) - item.startedAt) / 1000)
}

function transferEta(item: TransferItem) {
  if (item.paused) return t('sftp.statusPaused')
  if (item.status !== 'running') return t('sftp.seconds', { seconds: 0 })
  if (!item.total || item.loaded <= 0) return t('sftp.waitingEstimate')
  const elapsed = (Date.now() - item.startedAt) / 1000
  const speed = item.loaded / Math.max(0.001, elapsed)
  if (speed <= 0) return t('sftp.waitingEstimate')
  return formatDuration((item.total - item.loaded) / speed)
}

function formatDuration(seconds: number) {
  const total = Math.max(0, Math.round(seconds))
  const minutes = Math.floor(total / 60)
  const rest = total % 60
  if (minutes <= 0) return t('sftp.seconds', { seconds: rest })
  const hours = Math.floor(minutes / 60)
  const mins = minutes % 60
  if (hours <= 0) return t('sftp.minutesSeconds', { minutes, seconds: rest })
  return t('sftp.hoursMinutes', { hours, minutes: mins })
}

async function withHostKey<T>(path: string, payload: Record<string, unknown>) {
  try {
    return await postJSON<T>(path, payload)
  } catch (error) {
    const err = error as APIError
    const hostKey = err.data?.host_key as HostKeyInfo | undefined
    if (!hostKey || hostKey.mismatch) throw error
    await ElMessageBox.confirm(
      t('launcher.hostKeyConfirm', {
        address: hostKey.address,
        keyType: hostKey.key_type,
        fingerprint: hostKey.fingerprint,
        path: hostKey.known_hosts_path
      }),
      t('launcher.hostKeyTitle'),
      { type: 'warning' }
    )
    return await postJSON<T>(path, { ...payload, accept_host_key: true })
  }
}

function visibleEntries(paneName: PaneName) {
  return visibleEntriesByPane.value[paneName]
}

function visibleEntriesForPane(paneName: PaneName) {
  const tab = activeTab(paneName)
  if (!tab) return []
  const entries = tab.showHidden ? tab.entries : tab.entries.filter(entry => !entry.name.startsWith('.'))
  return sortEntries(paneName, entries)
}

function rawFileEntries(entries: FileEntry[] | undefined): FileEntry[] {
  return markRaw(Array.isArray(entries) ? entries : [])
}

function parentSlotHeight(paneName: PaneName) {
  return parentDirectoryEntry(paneName) ? FILE_ROW_SLOT_HEIGHT : 0
}

function virtualRange(paneName: PaneName) {
  const count = visibleEntries(paneName).length
  if (count === 0) return { start: 0, end: 0 }

  const pane = panes[paneName]
  const viewportHeight = pane.bodyViewportHeight || pane.bodyRef?.clientHeight || 0
  const itemScrollTop = Math.max(0, pane.bodyScrollTop - parentSlotHeight(paneName))
  const visibleSlots = Math.max(1, Math.ceil(viewportHeight / FILE_ROW_SLOT_HEIGHT))
  const firstVisible = clamp(Math.floor(itemScrollTop / FILE_ROW_SLOT_HEIGHT), 0, Math.max(0, count - 1))
  const lastVisible = Math.min(count, firstVisible + visibleSlots)

  return {
    start: clamp(firstVisible - VIRTUAL_ROW_OVERSCAN, 0, count),
    end: clamp(lastVisible + VIRTUAL_ROW_OVERSCAN, 0, count)
  }
}

function virtualRows(paneName: PaneName): VirtualFileRow[] {
  const entries = visibleEntries(paneName)
  const range = virtualRange(paneName)
  return entries.slice(range.start, range.end).map((entry, offset) => ({
    entry,
    index: range.start + offset
  }))
}

function topSpacerHeight(paneName: PaneName) {
  return virtualRange(paneName).start * FILE_ROW_SLOT_HEIGHT
}

function bottomSpacerHeight(paneName: PaneName) {
  const range = virtualRange(paneName)
  return Math.max(0, (visibleEntries(paneName).length - range.end) * FILE_ROW_SLOT_HEIGHT)
}

function sortEntries(paneName: PaneName, entries: FileEntry[]) {
  const pane = panes[paneName]
  return [...entries].sort((a, b) => {
    const typeCompare = entryTypeRank(a) - entryTypeRank(b)
    if (typeCompare !== 0) return typeCompare
    const result = compareEntries(paneName, a, b)
    return pane.sortDirection === 'asc' ? result : -result
  })
}

function entryTypeRank(entry: FileEntry) {
  return entry.is_dir ? 0 : 1
}

function fileIconURL(entry: FileEntry) {
  const params = new URLSearchParams({
    name: entry.is_dir ? 'folder' : entry.name || entry.path,
    is_dir: entry.is_dir ? '1' : '0',
    size: '16'
  })
  return `/api/file-icon?${params.toString()}`
}

function parentDirectoryIconURL(paneName: PaneName) {
  return fileIconURL(parentDirectoryEntry(paneName) || {
    name: '..',
    path: '..',
    is_dir: true,
    size: 0,
    mode: '',
    mod_time: ''
  })
}

function systemIconFailed(entry: FileEntry) {
  return failedSystemIconKeys.value.has(systemIconKey(entry))
}

function markSystemIconFailed(entry: FileEntry) {
  failedSystemIconKeys.value = new Set([...failedSystemIconKeys.value, systemIconKey(entry)])
}

function markSystemIconLoaded(entry: FileEntry) {
  clearSystemIconFailure(systemIconKey(entry))
}

function parentDirectoryIconFailed(paneName: PaneName) {
  const entry = parentDirectoryEntry(paneName)
  return entry ? systemIconFailed(entry) : false
}

function markParentDirectoryIconLoaded(paneName: PaneName) {
  const entry = parentDirectoryEntry(paneName)
  if (entry) markSystemIconLoaded(entry)
}

function markParentDirectoryIconFailed(paneName: PaneName) {
  const entry = parentDirectoryEntry(paneName)
  if (entry) markSystemIconFailed(entry)
}

function systemIconKey(entry: FileEntry) {
  if (entry.is_dir) return 'dir'
  const pathKey = (entry.path || entry.name || '').trim().toLowerCase()
  const name = (entry.name || entry.path || '').trim().toLowerCase()
  const lastSlash = Math.max(pathKey.lastIndexOf('/'), pathKey.lastIndexOf('\\'))
  const base = lastSlash >= 0 ? pathKey.slice(lastSlash + 1) : name
  const dot = base.lastIndexOf('.')
  const ext = dot >= 0 ? base.slice(dot) : 'file'
  return `${ext}:${pathKey || name}`
}

function clearSystemIconFailure(key: string) {
  if (!failedSystemIconKeys.value.has(key)) return
  const next = new Set(failedSystemIconKeys.value)
  next.delete(key)
  failedSystemIconKeys.value = next
}

function compareEntries(paneName: PaneName, a: FileEntry, b: FileEntry) {
  const key = panes[paneName].sortKey
  let result = 0
  if (key === 'name') {
    result = nameCompare(a.name, b.name)
  } else if (key === 'size') {
    result = (a.is_dir ? -1 : a.size ?? -1) - (b.is_dir ? -1 : b.size ?? -1)
  } else {
    result = timestamp(a.mod_time) - timestamp(b.mod_time)
  }
  return result === 0 ? nameCompare(a.name, b.name) : result
}

function nameCompare(a: string, b: string) {
  return fileNameCollator.value.compare(a, b)
}

function timestamp(value: string) {
  const time = new Date(value).getTime()
  return Number.isNaN(time) ? 0 : time
}

function sortAria(paneName: PaneName, key: SortKey) {
  const pane = panes[paneName]
  if (pane.sortKey !== key) return 'none'
  return pane.sortDirection === 'asc' ? 'ascending' : 'descending'
}

function sortTitle(paneName: PaneName, key: SortKey) {
  const field = key === 'name' ? '名称' : key === 'size' ? '大小' : '修改时间'
  const pane = panes[paneName]
  if (pane.sortKey !== key) return `按${field}排序`
  return `按${field}排序：${pane.sortDirection === 'asc' ? '升序' : '降序'}`
}

function toggleSort(paneName: PaneName, key: SortKey) {
  const pane = panes[paneName]
  if (pane.sortKey === key) {
    pane.sortDirection = pane.sortDirection === 'asc' ? 'desc' : 'asc'
  } else {
    pane.sortKey = key
    pane.sortDirection = 'asc'
  }
  nextTick(() => updateVirtualMetrics(paneName))
}

function selectedEntries(paneName: PaneName) {
  return activeTab(paneName)?.selected || []
}

function isSelected(paneName: PaneName, entry: FileEntry) {
  return selectedEntries(paneName).some(item => item.path === entry.path)
}

function isDragSource(paneName: PaneName, entry: FileEntry) {
  return Boolean(
    (dragging.value?.pane === paneName && dragging.value.paths.includes(entry.path)) ||
    exportDragPathSet.value.has(entry.path)
  )
}

function columnStyle(paneName: PaneName) {
  const pane = panes[paneName]
  return {
    '--file-size-col': `${pane.sizeColumnWidth}px`,
    '--file-time-col': `${pane.timeColumnWidth}px`
  }
}

function isColumnResizing(paneName: PaneName) {
  return activeColumnResize.value?.pane === paneName
}

function updateVirtualMetrics(paneName: PaneName) {
  const pane = panes[paneName]
  const body = pane.bodyRef
  if (!body) return
  pane.bodyScrollTop = body.scrollTop
  pane.bodyViewportHeight = body.clientHeight
  updateScrollbars(paneName)
}

function handleBodyScroll(paneName: PaneName) {
  updateVirtualMetrics(paneName)
}

function clamp(value: number, min: number, max: number) {
  if (max < min) return min
  return Math.min(Math.max(value, min), max)
}

function scrollStateForElement(body: HTMLElement) {
  const showVertical = body.scrollHeight > body.clientHeight + 1
  const showHorizontal = body.scrollWidth > body.clientWidth + 1
  const verticalTrack = Math.max(0, body.clientHeight - (showHorizontal ? 12 : 0))
  const horizontalTrack = Math.max(0, body.clientWidth - (showVertical ? 12 : 0))
  const maxScrollTop = Math.max(0, body.scrollHeight - body.clientHeight)
  const maxScrollLeft = Math.max(0, body.scrollWidth - body.clientWidth)
  const verticalThumbSize = showVertical
    ? clamp((body.clientHeight / body.scrollHeight) * verticalTrack, FLOATING_SCROLLBAR_MIN_THUMB, verticalTrack)
    : FLOATING_SCROLLBAR_MIN_THUMB
  const horizontalThumbSize = showHorizontal
    ? clamp((body.clientWidth / body.scrollWidth) * horizontalTrack, FLOATING_SCROLLBAR_MIN_THUMB, horizontalTrack)
    : FLOATING_SCROLLBAR_MIN_THUMB
  const verticalMaxOffset = Math.max(0, verticalTrack - verticalThumbSize)
  const horizontalMaxOffset = Math.max(0, horizontalTrack - horizontalThumbSize)

  return {
    showVertical,
    showHorizontal,
    verticalThumbSize,
    verticalThumbOffset: maxScrollTop <= 0 ? 0 : (body.scrollTop / maxScrollTop) * verticalMaxOffset,
    horizontalThumbSize,
    horizontalThumbOffset: maxScrollLeft <= 0 ? 0 : (body.scrollLeft / maxScrollLeft) * horizontalMaxOffset
  } satisfies ScrollState
}

function isSameScrollState(current: ScrollState, nextState: ScrollState) {
  return (
    current.showVertical === nextState.showVertical &&
    current.showHorizontal === nextState.showHorizontal &&
    Math.abs(current.verticalThumbSize - nextState.verticalThumbSize) < 0.5 &&
    Math.abs(current.verticalThumbOffset - nextState.verticalThumbOffset) < 0.5 &&
    Math.abs(current.horizontalThumbSize - nextState.horizontalThumbSize) < 0.5 &&
    Math.abs(current.horizontalThumbOffset - nextState.horizontalThumbOffset) < 0.5
  )
}

function updateScrollbars(paneName: PaneName) {
  const body = panes[paneName].bodyRef
  if (!body) return
  const nextState = scrollStateForElement(body)
  const current = panes[paneName].scrollState
  if (isSameScrollState(current, nextState)) return
  panes[paneName].scrollState = nextState
}

function updateTransferScrollbars() {
  const body = transferListRef.value
  if (!body) return
  const nextState = scrollStateForElement(body)
  if (isSameScrollState(transferScrollState.value, nextState)) return
  transferScrollState.value = nextState
}

function startScrollbarThumbDrag(paneName: PaneName, axis: ScrollbarAxis, event: PointerEvent) {
  const body = panes[paneName].bodyRef
  if (!body) return
  event.preventDefault()
  const scrollState = panes[paneName].scrollState
  const trackLength = axis === 'vertical'
    ? Math.max(0, body.clientHeight - (scrollState.showHorizontal ? 12 : 0))
    : Math.max(0, body.clientWidth - (scrollState.showVertical ? 12 : 0))
  const thumbSize = axis === 'vertical' ? scrollState.verticalThumbSize : scrollState.horizontalThumbSize
  activeScrollbarDrag = {
    pane: paneName,
    axis,
    pointerId: event.pointerId,
    startClient: axis === 'vertical' ? event.clientY : event.clientX,
    startScroll: axis === 'vertical' ? body.scrollTop : body.scrollLeft,
    maxScroll: axis === 'vertical' ? Math.max(0, body.scrollHeight - body.clientHeight) : Math.max(0, body.scrollWidth - body.clientWidth),
    maxThumbOffset: Math.max(0, trackLength - thumbSize)
  }
  const target = event.currentTarget instanceof HTMLElement ? event.currentTarget : null
  target?.setPointerCapture(event.pointerId)
  window.addEventListener('pointermove', handleScrollbarThumbMove)
  window.addEventListener('pointerup', stopScrollbarThumbDrag, { once: true })
  window.addEventListener('pointercancel', stopScrollbarThumbDrag, { once: true })
}

function handleScrollbarThumbMove(event: PointerEvent) {
  const drag = activeScrollbarDrag
  if (!drag || event.pointerId !== drag.pointerId) return
  const body = panes[drag.pane].bodyRef
  if (!body) return
  event.preventDefault()
  const client = drag.axis === 'vertical' ? event.clientY : event.clientX
  const delta = client - drag.startClient
  const scrollDelta = drag.maxThumbOffset <= 0 ? 0 : (delta / drag.maxThumbOffset) * drag.maxScroll
  if (drag.axis === 'vertical') body.scrollTop = drag.startScroll + scrollDelta
  else body.scrollLeft = drag.startScroll + scrollDelta
  updateVirtualMetrics(drag.pane)
}

function stopScrollbarThumbDrag() {
  activeScrollbarDrag = null
  window.removeEventListener('pointermove', handleScrollbarThumbMove)
  window.removeEventListener('pointerup', stopScrollbarThumbDrag)
  window.removeEventListener('pointercancel', stopScrollbarThumbDrag)
}

function handleScrollbarTrackPointerDown(paneName: PaneName, axis: ScrollbarAxis, event: PointerEvent) {
  const body = panes[paneName].bodyRef
  if (!body || !(event.target instanceof HTMLElement) || event.target.classList.contains('file-floating-scrollbar-thumb')) return
  event.preventDefault()
  const rect = event.currentTarget instanceof HTMLElement ? event.currentTarget.getBoundingClientRect() : null
  if (!rect) return
  if (axis === 'vertical') {
    const ratio = clamp((event.clientY - rect.top) / rect.height, 0, 1)
    body.scrollTop = ratio * Math.max(0, body.scrollHeight - body.clientHeight)
  } else {
    const ratio = clamp((event.clientX - rect.left) / rect.width, 0, 1)
    body.scrollLeft = ratio * Math.max(0, body.scrollWidth - body.clientWidth)
  }
  updateVirtualMetrics(paneName)
}

function startTransferScrollbarThumbDrag(axis: ScrollbarAxis, event: PointerEvent) {
  const body = transferListRef.value
  if (!body) return
  event.preventDefault()
  const state = transferScrollState.value
  const trackLength = axis === 'vertical'
    ? Math.max(0, body.clientHeight - (state.showHorizontal ? 12 : 0))
    : Math.max(0, body.clientWidth - (state.showVertical ? 12 : 0))
  const thumbSize = axis === 'vertical' ? state.verticalThumbSize : state.horizontalThumbSize
  activeTransferScrollbarDrag = {
    axis,
    pointerId: event.pointerId,
    startClient: axis === 'vertical' ? event.clientY : event.clientX,
    startScroll: axis === 'vertical' ? body.scrollTop : body.scrollLeft,
    maxScroll: axis === 'vertical' ? Math.max(0, body.scrollHeight - body.clientHeight) : Math.max(0, body.scrollWidth - body.clientWidth),
    maxThumbOffset: Math.max(0, trackLength - thumbSize)
  }
  const target = event.currentTarget instanceof HTMLElement ? event.currentTarget : null
  target?.setPointerCapture(event.pointerId)
  window.addEventListener('pointermove', handleTransferScrollbarThumbMove)
  window.addEventListener('pointerup', stopTransferScrollbarThumbDrag, { once: true })
  window.addEventListener('pointercancel', stopTransferScrollbarThumbDrag, { once: true })
}

function handleTransferScrollbarThumbMove(event: PointerEvent) {
  const drag = activeTransferScrollbarDrag
  const body = transferListRef.value
  if (!drag || !body || event.pointerId !== drag.pointerId) return
  event.preventDefault()
  const client = drag.axis === 'vertical' ? event.clientY : event.clientX
  const delta = client - drag.startClient
  const scrollDelta = drag.maxThumbOffset <= 0 ? 0 : (delta / drag.maxThumbOffset) * drag.maxScroll
  if (drag.axis === 'vertical') body.scrollTop = drag.startScroll + scrollDelta
  else body.scrollLeft = drag.startScroll + scrollDelta
  updateTransferScrollbars()
}

function stopTransferScrollbarThumbDrag() {
  activeTransferScrollbarDrag = null
  window.removeEventListener('pointermove', handleTransferScrollbarThumbMove)
  window.removeEventListener('pointerup', stopTransferScrollbarThumbDrag)
  window.removeEventListener('pointercancel', stopTransferScrollbarThumbDrag)
}

function handleTransferScrollbarTrackPointerDown(axis: ScrollbarAxis, event: PointerEvent) {
  const body = transferListRef.value
  if (!body || !(event.target instanceof HTMLElement) || event.target.classList.contains('file-floating-scrollbar-thumb')) return
  event.preventDefault()
  const rect = event.currentTarget instanceof HTMLElement ? event.currentTarget.getBoundingClientRect() : null
  if (!rect) return
  if (axis === 'vertical') {
    const ratio = clamp((event.clientY - rect.top) / rect.height, 0, 1)
    body.scrollTop = ratio * Math.max(0, body.scrollHeight - body.clientHeight)
  } else {
    const ratio = clamp((event.clientX - rect.left) / rect.width, 0, 1)
    body.scrollLeft = ratio * Math.max(0, body.scrollWidth - body.clientWidth)
  }
  updateTransferScrollbars()
}

function maxSizeColumnWidth(paneName: PaneName) {
  const pane = panes[paneName]
  const width = pane.treeRef?.getBoundingClientRect().width ?? 0
  if (width <= 0) return MAX_SIZE_COLUMN_WIDTH
  const reserved = FILE_ICON_COLUMN_WIDTH + FILE_COLUMN_RESIZER_WIDTH * 2 + MIN_NAME_COLUMN_WIDTH + pane.timeColumnWidth
  return Math.min(MAX_SIZE_COLUMN_WIDTH, Math.max(MIN_SIZE_COLUMN_WIDTH, width - reserved))
}

function startColumnResize(paneName: PaneName, target: ColumnResizeTarget, event: PointerEvent) {
  event.preventDefault()
  event.stopPropagation()
  const pane = panes[paneName]
  activeColumnResize.value = { pane: paneName, target }
  columnResizeStartX = event.clientX
  columnResizeStartSize = pane.sizeColumnWidth
  columnResizeStartTime = pane.timeColumnWidth
  previousBodyCursor = document.body.style.cursor
  previousBodyUserSelect = document.body.style.userSelect
  document.body.style.cursor = 'col-resize'
  document.body.style.userSelect = 'none'
  window.addEventListener('pointermove', handleColumnResizeMove)
  window.addEventListener('pointerup', stopColumnResize, { once: true })
  window.addEventListener('pointercancel', stopColumnResize, { once: true })
}

function handleColumnResizeMove(event: PointerEvent) {
  const active = activeColumnResize.value
  if (!active) return
  const pane = panes[active.pane]
  const delta = event.clientX - columnResizeStartX

  if (active.target === 'name-size') {
    pane.sizeColumnWidth = clamp(columnResizeStartSize - delta, MIN_SIZE_COLUMN_WIDTH, maxSizeColumnWidth(active.pane))
    return
  }

  const combinedWidth = columnResizeStartSize + columnResizeStartTime
  const minSize = Math.max(MIN_SIZE_COLUMN_WIDTH, combinedWidth - MAX_TIME_COLUMN_WIDTH)
  const maxSize = Math.min(MAX_SIZE_COLUMN_WIDTH, combinedWidth - MIN_TIME_COLUMN_WIDTH, maxSizeColumnWidth(active.pane))
  const nextSize = clamp(columnResizeStartSize + delta, minSize, maxSize)
  pane.sizeColumnWidth = nextSize
  pane.timeColumnWidth = clamp(combinedWidth - nextSize, MIN_TIME_COLUMN_WIDTH, MAX_TIME_COLUMN_WIDTH)
}

function stopColumnResize() {
  if (!activeColumnResize.value) return
  activeColumnResize.value = null
  document.body.style.cursor = previousBodyCursor
  document.body.style.userSelect = previousBodyUserSelect
  window.removeEventListener('pointermove', handleColumnResizeMove)
  window.removeEventListener('pointerup', stopColumnResize)
  window.removeEventListener('pointercancel', stopColumnResize)
}

function resetColumnWidths(paneName: PaneName) {
  panes[paneName].sizeColumnWidth = DEFAULT_SIZE_COLUMN_WIDTH
  panes[paneName].timeColumnWidth = DEFAULT_TIME_COLUMN_WIDTH
}

function toggleHidden(paneName: PaneName) {
  const tab = activeTab(paneName)
  if (!tab) return
  tab.showHidden = !tab.showHidden
  if (!tab.showHidden) {
    tab.selected = tab.selected.filter(entry => !entry.name.startsWith('.'))
    if (tab.anchorPath && !visibleEntries(paneName).some(entry => entry.path === tab.anchorPath)) tab.anchorPath = ''
  }
  nextTick(() => updateVirtualMetrics(paneName))
}

async function openDraftTab(paneName: PaneName) {
  const pane = panes[paneName]
  if (pane.draft.kind === 'local') {
    await openLocalTab(paneName, pane.draft.localPath || home.value)
    return
  }
  const profile = remoteProfiles.value.find(item => item.id === pane.draft.profileID)
  if (!profile) {
    ElMessage.warning('请先选择已保存的服务器')
    return
  }
  await openRemoteTab(paneName, profile, pane.draft.remotePath || '.')
}

async function openLocalTab(paneName: PaneName, path: string) {
  const pane = panes[paneName]
  const tab = localTab(path)
  pane.tabs.push(tab)
  pane.active = tab.id
  pane.newMenuOpen = false
  await loadTab(paneName, tabByID(paneName, tab.id))
}

async function openRemoteTab(paneName: PaneName, profile: Profile, path: string) {
  const pane = panes[paneName]
  const tab = pendingRemoteTab(profile, path)
  pane.tabs.push(tab)
  pane.active = tab.id
  pane.newMenuOpen = false
  log(`连接 SFTP：${profile.name || profile.ssh_host}`)
  try {
    const session = await withHostKey<{ id: string }>('/api/sftp/connect', { id: profile.id })
    const liveTab = tabByID(paneName, tab.id)
    if (!liveTab) {
      await postJSON('/api/sftp/disconnect', { session: session.id }).catch(() => undefined)
      return
    }
    liveTab.session = session.id
    await loadTab(paneName, liveTab)
  } catch (error) {
    const liveTab = tabByID(paneName, tab.id)
    if (liveTab) {
      liveTab.loading = false
      liveTab.title = `${profile.name || '远程'}:连接失败`
    }
    ElMessage.error((error as Error).message)
    log(`失败：${(error as Error).message}`)
  }
}

async function closeTab(paneName: PaneName, tabID: string) {
  const pane = panes[paneName]
  if (pane.tabs.length <= 1) {
    ElMessage.warning('每个窗口至少保留一个标签页')
    return
  }
  const tab = pane.tabs.find(item => item.id === tabID)
  pane.tabs = pane.tabs.filter(item => item.id !== tabID)
  if (pane.active === tabID) pane.active = pane.tabs[0]?.id || ''
  if (tab?.kind === 'remote' && tab.session) {
    await postJSON('/api/sftp/disconnect', { session: tab.session }).catch(() => undefined)
  }
}

async function loadTab(paneName: PaneName, tab = activeTab(paneName), options: { recordHistory?: boolean } = {}) {
  if (!tab) return
  const previousPath = tab.loadedPath || tab.path
  tab.loading = true
  tab.selected = []
  tab.anchorPath = ''
  try {
    const data = tab.kind === 'local'
      ? await api<{ path: string; entries: FileEntry[] }>(`/api/local/list?path=${encodeURIComponent(tab.path)}`)
      : await api<{ path: string; entries: FileEntry[] }>(`/api/sftp/list?session=${encodeURIComponent(tab.session || '')}&path=${encodeURIComponent(tab.path)}`)
    const nextPath = data.path || tab.path
    tab.path = nextPath
    tab.loadedPath = nextPath
    tab.entries = rawFileEntries(data.entries)
    if (options.recordHistory) recordDirectoryHistory(tab, previousPath, nextPath)
    tab.title = tabLabel(tab)
    clearSystemIconFailure('dir')
  } catch (error) {
    ElMessage.error((error as Error).message)
    log(`失败：${(error as Error).message}`)
  } finally {
    tab.loading = false
    nextTick(() => updateVirtualMetrics(paneName))
  }
}

function setPath(paneName: PaneName, path: string) {
  const tab = activeTab(paneName)
  if (tab) tab.path = path
}

async function goPath(paneName: PaneName) {
  await loadTab(paneName, activeTab(paneName), { recordHistory: true })
}

async function openParentDirectory(paneName: PaneName) {
  const tab = activeTab(paneName)
  if (!tab) return
  const parent = parentPath(tab.loadedPath || tab.path, tab.kind)
  if (!parent || parent === (tab.loadedPath || tab.path)) return
  tab.path = parent
  await loadTab(paneName, tab, { recordHistory: true })
}

function parentDirectoryEntry(paneName: PaneName) {
  const tab = activeTab(paneName)
  if (!tab) return null
  const current = tab.loadedPath || tab.path
  const parent = parentPath(current, tab.kind)
  if (!parent || parent === current) return null
  return {
    name: '..',
    path: parent,
    is_dir: true,
    size: 0,
    mode: '',
    mod_time: ''
  } satisfies FileEntry
}

function canGoBack(paneName: PaneName) {
  return Boolean(activeTab(paneName)?.history.length)
}

function historyEntries(paneName: PaneName) {
  return (activeTab(paneName)?.history || []).map(path => ({ path, label: path }))
}

async function goBack(paneName: PaneName) {
  const tab = activeTab(paneName)
  const path = tab?.history[0]
  if (!tab || !path) return
  tab.history = tab.history.slice(1)
  tab.path = path
  await loadTab(paneName, tab)
}

async function openHistoryPath(paneName: PaneName, path: string) {
  const tab = activeTab(paneName)
  if (!tab || !path) return
  tab.history = tab.history.filter(item => item !== path)
  tab.path = path
  await loadTab(paneName, tab)
}

function handleHistoryCommand(paneName: PaneName, command: string | number | boolean) {
  void openHistoryPath(paneName, String(command))
}

function recordDirectoryHistory(tab: FileTab, previousPath: string, nextPath: string) {
  if (!previousPath || previousPath === nextPath) return
  tab.history = [previousPath, ...tab.history.filter(path => path !== previousPath)].slice(0, DIRECTORY_HISTORY_LIMIT)
}

function beginSelect(paneName: PaneName, index: number, entry: FileEntry, event: MouseEvent) {
  const tab = activeTab(paneName)
  if (!tab) return
  event.stopPropagation()
  finishFileDrag()
  const items = visibleEntries(paneName)
  if (event.shiftKey) {
    event.preventDefault()
    const anchorIndex = Math.max(0, items.findIndex(item => item.path === (tab.anchorPath || tab.selected[0]?.path || entry.path)))
    selectRange(paneName, anchorIndex, index)
    return
  }
  if (event.ctrlKey || event.metaKey) {
    event.preventDefault()
    if (isSelected(paneName, entry)) {
      tab.selected = tab.selected.filter(item => item.path !== entry.path)
    } else {
      tab.selected = [...tab.selected, entry]
    }
    tab.anchorPath = entry.path
    return
  }
  if (isSelected(paneName, entry) && tab.selected.length > 1) {
    tab.anchorPath = entry.path
    startLongPress(paneName, index, event)
    return
  }
  tab.selected = [entry]
  tab.anchorPath = entry.path
  startLongPress(paneName, index, event)
}

function selectRange(paneName: PaneName, start: number, end: number) {
  const tab = activeTab(paneName)
  if (!tab) return
  const items = visibleEntries(paneName)
  const from = Math.min(start, end)
  const to = Math.max(start, end)
  tab.selected = items.slice(from, to + 1)
  tab.anchorPath = items[start]?.path || items[end]?.path || ''
}

function startLongPress(paneName: PaneName, index: number, event: MouseEvent) {
  cancelLongPress()
  pressPane = paneName
  pressIndex = index
  pressStartX = event.clientX
  pressStartY = event.clientY
  longPressTimer = window.setTimeout(() => armFileDrag(paneName, index), LONG_PRESS_MS)
  window.addEventListener('mousemove', handlePressMove)
}

function cancelLongPress() {
  if (longPressTimer !== null) {
    window.clearTimeout(longPressTimer)
    longPressTimer = null
  }
}

function resetPressState() {
  cancelLongPress()
  pressPane = null
  pressIndex = null
  window.removeEventListener('mousemove', handlePressMove)
}

function handlePressMove(event: MouseEvent) {
  if (pressPane === null || pressIndex === null || exportDragArmed.value) return
  if (Math.abs(event.clientX - pressStartX) < SELECT_DRAG_THRESHOLD && Math.abs(event.clientY - pressStartY) < SELECT_DRAG_THRESHOLD) return
  cancelLongPress()
  rangeDragPane.value = pressPane
  rangeDragAnchor.value = pressIndex
  rangeDragOver.value = pressIndex
}

function armFileDrag(paneName: PaneName, index: number) {
  const items = dragItemsForIndex(paneName, index)
  exportDragPathSet.value = new Set(items.map(item => item.path))
  exportDragArmed.value = items.length > 0
  rangeDragPane.value = null
  rangeDragAnchor.value = null
  rangeDragOver.value = null
}

function dragItemsForIndex(paneName: PaneName, index: number) {
  const tab = activeTab(paneName)
  const item = visibleEntries(paneName)[index]
  if (!tab || !item) return []
  if (tab.selected.some(selected => selected.path === item.path) && tab.selected.length > 0) {
    const selected = new Set(tab.selected.map(entry => entry.path))
    return visibleEntries(paneName).filter(entry => selected.has(entry.path))
  }
  return [item]
}

function extendSelection(paneName: PaneName, index: number) {
  if (pressPane === paneName && pressIndex !== null && !exportDragArmed.value && rangeDragPane.value === null && index !== pressIndex) {
    cancelLongPress()
    rangeDragPane.value = paneName
    rangeDragAnchor.value = pressIndex
    rangeDragOver.value = pressIndex
  }
  if (rangeDragPane.value !== paneName || rangeDragAnchor.value === null) return
  rangeDragOver.value = index
  selectRange(paneName, rangeDragAnchor.value, index)
}

function beginBlankSelect(paneName: PaneName, event: MouseEvent) {
  const target = event.target
  if (target instanceof Element && target.closest('.file-row')) return
  event.preventDefault()
  resetPressState()
  blankDragPane.value = paneName
  blankDragStartX = event.clientX
  blankDragStartY = event.clientY
  blankDragCurrentX = event.clientX
  blankDragCurrentY = event.clientY
  updateBlankDragSelection()
  window.addEventListener('mousemove', handleBlankDragMove)
}

function handleBlankDragMove(event: MouseEvent) {
  if (!blankDragPane.value) return
  event.preventDefault()
  blankDragCurrentX = event.clientX
  blankDragCurrentY = event.clientY
  updateBlankDragSelection()
}

function updateBlankDragSelection() {
  const paneName = blankDragPane.value
  if (!paneName) return
  const tab = activeTab(paneName)
  if (!tab) return
  const left = Math.min(blankDragStartX, blankDragCurrentX)
  const right = Math.max(blankDragStartX, blankDragCurrentX)
  const top = Math.min(blankDragStartY, blankDragCurrentY)
  const bottom = Math.max(blankDragStartY, blankDragCurrentY)
  const selectedPaths = new Set<string>()
  const selectedItems: FileEntry[] = []
  const items = visibleEntries(paneName)

  const body = panes[paneName].bodyRef
  if (body && items.length > 0) {
    const bodyRect = body.getBoundingClientRect()
    const contentTop = bodyRect.top + FILE_BODY_VERTICAL_PADDING + parentSlotHeight(paneName) - body.scrollTop
    const contentBottom = contentTop + items.length * FILE_ROW_SLOT_HEIGHT
    const intersectsHorizontally = right >= bodyRect.left && left <= bodyRect.right

    if (intersectsHorizontally && bottom >= contentTop && top <= contentBottom) {
      const firstIndex = clamp(Math.floor((top - contentTop) / FILE_ROW_SLOT_HEIGHT), 0, items.length - 1)
      const lastIndex = clamp(Math.floor((bottom - contentTop) / FILE_ROW_SLOT_HEIGHT), 0, items.length - 1)

      for (let index = firstIndex; index <= lastIndex; index += 1) {
        const entry = items[index]
        if (!entry) continue
        selectedPaths.add(entry.path)
        selectedItems.push(entry)
      }
    }
  }

  blankDragPathSet.value = selectedPaths
  tab.selected = selectedItems
  tab.anchorPath = tab.selected[0]?.path || ''
}

function finishSelection() {
  if (blankDragPane.value) {
    window.removeEventListener('mousemove', handleBlankDragMove)
  }
  rangeDragPane.value = null
  rangeDragAnchor.value = null
  rangeDragOver.value = null
  blankDragPane.value = null
  blankDragPathSet.value = new Set()
  if (!exportDragActive.value) finishFileDrag()
  resetPressState()
}

function isInDragRange(paneName: PaneName, index: number, entry: FileEntry) {
  if (blankDragPane.value === paneName && blankDragPathSet.value.has(entry.path)) return true
  if (rangeDragPane.value !== paneName || rangeDragAnchor.value === null || rangeDragOver.value === null) return false
  const from = Math.min(rangeDragAnchor.value, rangeDragOver.value)
  const to = Math.max(rangeDragAnchor.value, rangeDragOver.value)
  return index >= from && index <= to
}

function openFileContextMenu(paneName: PaneName, index: number, entry: FileEntry, event: MouseEvent) {
  const tab = activeTab(paneName)
  if (!tab) return
  event.preventDefault()
  event.stopPropagation()
  closeTransferContextMenu()
  if (!isSelected(paneName, entry)) {
    tab.selected = [entry]
    tab.anchorPath = entry.path
  }
  resetPressState()
  const menuWidth = 210
  const menuHeight = 220
  contextMenu.value = {
    visible: true,
    x: Math.min(event.clientX, Math.max(8, window.innerWidth - menuWidth - 8)),
    y: Math.min(event.clientY, Math.max(8, window.innerHeight - menuHeight - 8)),
    pane: paneName,
    entry
  }
}

function closeContextMenu() {
  if (contextMenu.value.visible) {
    contextMenu.value = { visible: false, x: 0, y: 0, pane: null, entry: null }
  }
  closeTransferContextMenu()
}

async function runContextAction(action: 'open' | 'openText' | 'copyPath' | 'rename' | 'delete') {
  const paneName = contextMenu.value.pane
  const entry = contextMenu.value.entry
  closeContextMenu()
  if (!paneName || !entry) return
  if (action === 'open') {
    await openEntry(paneName, entry)
  } else if (action === 'openText') {
    await openTextFile(paneName, entry)
  } else if (action === 'copyPath') {
    await copyEntryPath(entry)
  } else if (action === 'rename') {
    await renameEntry(paneName, entry)
  } else if (action === 'delete') {
    const tab = activeTab(paneName)
    if (tab && !isSelected(paneName, entry)) tab.selected = [entry]
    await deleteSelected(paneName)
  }
}

function openTransferContextMenu(item: TransferItem, event: MouseEvent) {
  event.preventDefault()
  event.stopPropagation()
  if (!selectedTransferIDs.value.has(item.id)) {
    selectedTransferIDs.value = new Set([item.id])
    transferAnchorID.value = item.id
  }
  const menuWidth = 180
  const menuHeight = 150
  contextMenu.value = { visible: false, x: 0, y: 0, pane: null, entry: null }
  transferContextMenu.value = {
    visible: true,
    x: Math.min(event.clientX, Math.max(8, window.innerWidth - menuWidth - 8)),
    y: Math.min(event.clientY, Math.max(8, window.innerHeight - menuHeight - 8)),
    item
  }
}

function closeTransferContextMenu() {
  if (!transferContextMenu.value.visible) return
  transferContextMenu.value = { visible: false, x: 0, y: 0, item: null }
}

function transferControlTargets(item: TransferItem | null) {
  const roots = selectedTransferTargets(item)
  const targets: TransferItem[] = []
  for (const root of roots) {
    if (!root.isGroup) {
      targets.push(root)
      continue
    }
    targets.push(...transferItems.value.filter(child => child.parentID === root.id))
  }
  const seen = new Set<string>()
  return targets.filter(target => {
    if (seen.has(target.id)) return false
    seen.add(target.id)
    return true
  })
}

function canPauseTransfer(item: TransferItem | null) {
  return transferControlTargets(item).some(target => target.status === 'running' && !target.paused)
}

function canResumeTransfer(item: TransferItem | null) {
  return transferControlTargets(item).some(target => target.status === 'running' && target.paused)
}

function canCancelTransfer(item: TransferItem | null) {
  return transferControlTargets(item).some(target => target.status === 'running')
}

async function runTransferContextAction(action: 'pause' | 'resume' | 'cancel') {
  const item = transferContextMenu.value.item
  const targets = transferControlTargets(item).filter(target => {
    if (action === 'pause') return target.status === 'running' && !target.paused
    if (action === 'resume') return target.status === 'running' && target.paused
    return target.status === 'running'
  })
  closeTransferContextMenu()
  if (targets.length === 0) return
  await Promise.all(targets.map(async target => {
    await postJSON('/api/transfer-control', { id: target.id, action })
    if (action === 'pause') {
      target.paused = true
    } else if (action === 'resume') {
      target.paused = false
    } else {
      target.paused = false
      target.cancelled = true
      finishTransferItem(target, 'error', t('sftp.transferCancelled'))
    }
    if (target.parentID) syncTransferGroup(target.parentID)
  }))
  for (const selected of selectedTransferTargets(item)) {
    if (selected.isGroup) syncTransferGroup(selected.id)
    if (selected.parentID) syncTransferGroup(selected.parentID)
  }
  touchTransferItems()
  nextTick(updateTransferScrollbars)
}

async function openEntry(paneName: PaneName, entry: FileEntry) {
  const tab = activeTab(paneName)
  if (!tab) return
  if (!entry.is_dir) {
    await openFile(paneName, entry)
    return
  }
  tab.path = entry.path
  await loadTab(paneName, tab, { recordHistory: true })
}

async function createFolder(paneName: PaneName) {
  const tab = activeTab(paneName)
  const name = await promptEntryName('新建文件夹', '文件夹名称')
  if (!tab || !name) return
  const path = joinPath(tab.path, name, tab.kind)
  if (tab.kind === 'local') {
    await postJSON('/api/local/mkdir', { path })
  } else {
    await postJSON('/api/sftp/mkdir', { session: tab.session, path })
  }
  log(`创建目录：${path}`)
  await loadTab(paneName, tab)
}

async function createFile(paneName: PaneName) {
  const tab = activeTab(paneName)
  const name = await promptEntryName('新建文件', '文件名称')
  if (!tab || !name) return
  const path = joinPath(tab.path, name, tab.kind)
  if (tab.kind === 'local') {
    await postJSON('/api/local/create-file', { path })
  } else {
    await postJSON('/api/sftp/create-file', { session: tab.session, path })
  }
  log(`创建文件：${path}`)
  await loadTab(paneName, tab)
}

async function promptEntryName(title: string, message: string) {
  let result: { value: string }
  try {
    result = await ElMessageBox.prompt(message, title, {
      inputPattern: safeNamePattern,
      inputErrorMessage: '名称不能包含路径分隔符',
      confirmButtonText: '确定',
      cancelButtonText: '取消'
    })
  } catch {
    return ''
  }
  return result.value.trim()
}

async function deleteSelected(paneName: PaneName) {
  const tab = activeTab(paneName)
  const selected = selectedEntries(paneName)
  if (!tab || selected.length === 0) {
    ElMessage.warning('请先选择文件或目录')
    return
  }
  const directories = selected.filter(entry => entry.is_dir)
  if (directories.length > 0) {
    const summary = await inspectDeleteDirectories(tab, directories)
    const emptyText = summary.empty.length > 0
      ? `<p class="delete-confirm-note">检测到 ${summary.empty.length} 个文件夹为空。</p>`
      : ''
    const nonEmptyText = summary.nonEmpty.length > 0
      ? `<div class="delete-confirm-danger"><strong>检测到 ${summary.nonEmpty.length} 个非空文件夹</strong><span>这些文件夹会连同内部所有文件和子文件夹一起删除。</span>${deleteConfirmList(summary.nonEmpty)}</div>`
      : ''
    const unknownText = summary.unknown.length > 0
      ? `<p class="delete-confirm-note">有 ${summary.unknown.length} 个文件夹无法确认是否为空，将按文件夹删除处理。</p>`
      : ''
    const title = summary.nonEmpty.length > 0 ? '递归删除确认' : '确认删除'
    await ElMessageBox.confirm(
      `<div class="delete-confirm-content"><p>将删除选中的 <strong>${selected.length}</strong> 项，其中包含 <strong>${directories.length}</strong> 个文件夹。</p>${emptyText}${nonEmptyText}${unknownText}<p class="delete-confirm-tail">此操作不可撤销。</p></div>`,
      title,
      {
        dangerouslyUseHTMLString: true,
        type: summary.nonEmpty.length > 0 ? 'error' : 'warning',
        confirmButtonText: summary.nonEmpty.length > 0 ? '递归删除' : '删除',
        cancelButtonText: '取消',
        confirmButtonClass: summary.nonEmpty.length > 0 ? 'el-button--danger' : undefined
      }
    )
  } else {
    await ElMessageBox.confirm(`删除选中的 ${selected.length} 项？此操作不可撤销。`, '确认删除', {
      type: 'warning',
      confirmButtonText: '删除',
      cancelButtonText: '取消'
    })
  }
  for (const entry of selected) {
    if (tab.kind === 'local') {
      await postJSON('/api/local/delete', { path: entry.path })
    } else {
      await postJSON('/api/sftp/delete', { session: tab.session, path: entry.path })
    }
    log(`删除：${entry.path}`)
  }
  await loadTab(paneName, tab)
}

async function inspectDeleteDirectories(tab: FileTab, directories: FileEntry[]) {
  const empty: FileEntry[] = []
  const nonEmpty: FileEntry[] = []
  const unknown: FileEntry[] = []
  for (const entry of directories) {
    try {
      const listing = await listTabDirectory(tab, entry.path)
      if (listing.entries.length > 0) nonEmpty.push(entry)
      else empty.push(entry)
    } catch {
      unknown.push(entry)
    }
  }
  return { empty, nonEmpty, unknown }
}

function deleteConfirmList(entries: FileEntry[]) {
  const shown = entries.slice(0, 4).map(entry => `<li>${escapeHTML(entry.name || entry.path)}</li>`).join('')
  const more = entries.length > 4 ? `<li>以及另外 ${entries.length - 4} 个文件夹...</li>` : ''
  return `<ul>${shown}${more}</ul>`
}

function escapeHTML(value: string) {
  return value
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;')
}

async function transferEntries(sourcePane: PaneName, targetPane: PaneName, entries: FileEntry[], targetPathOverride?: string) {
  const source = activeTab(sourcePane)
  const target = activeTab(targetPane)
  if (!source || !target || entries.length === 0) return
  const targetBasePath = targetPathOverride || target.path
  const expanded = await expandTransferEntries(source, entries)
  const normalized = normalizeTransferTasks(expanded)
  if (normalized.invalidCount > 0) ElMessage.error(`已跳过 ${normalized.invalidCount} 个不安全路径`)
  if (normalized.renamedCount > 0) ElMessage.info(`已将 ${normalized.renamedCount} 个传输路径中的空格重命名为下划线`)
  const prepared = await prepareRelativeTransfers(normalized.tasks, target, targetBasePath)
  if (prepared.cancelled || prepared.tasks.length === 0) return
  const ordered = orderTransferTasks(prepared.tasks)
  const transferGroups = createTransferGroups(source, target, prepared.tasks, targetBasePath)
  const fileTasks = ordered.filter(task => !task.isDir)
  for (const task of ordered.filter(task => task.isDir)) {
    const group = transferGroups.get(task.rootName)
    try {
      await ensureTargetDirectory(target, task.relativePath, targetBasePath)
    } catch (error) {
      if (group) finishTransferItem(group, 'error', (error as Error).message)
      throw error
    }
  }

  const itemsByTask = new Map<RelativeTransfer<FileEntry>, TransferItem>()
  for (const task of fileTasks) {
    const group = transferGroups.get(task.rootName)
    const targetPath = joinPath(targetBasePath, task.relativePath, target.kind)
    const item = createTransferItem({
      name: task.displayPath,
      kind: 'file',
      sourceName: transferEndpointName(source),
      targetName: transferEndpointName(target),
      sourcePath: task.payload.path,
      targetPath,
      total: task.payload.size || 0,
      parentID: group?.id
    })
    if (group) syncTransferGroup(group.id)
    itemsByTask.set(task, item)
  }

  for (const task of fileTasks) {
    const item = itemsByTask.get(task)
    if (!item) continue
    if (item.cancelled || item.status !== 'running') {
      if (item.parentID) syncTransferGroup(item.parentID)
      continue
    }
    const targetPath = joinPath(targetBasePath, task.relativePath, target.kind)
    log(`开始传输：${task.payload.path} -> ${targetPath}`)
    try {
      await runWithTransferProgress(item, item.id, () => transferFileTask(source, target, task, targetBasePath, item.id))
      finishTransferItem(item, 'done')
      log(`完成传输：${task.displayPath}`)
    } catch (error) {
      finishTransferItem(item, 'error', (error as Error).message)
      if (!isTransferCancelledError(error)) throw error
      log(`取消传输：${task.displayPath}`)
    }
  }
  finishEmptyTransferGroups(transferGroups)
  await loadTab(targetPane, target)
}

function createTransferGroups(source: FileTab, target: FileTab, tasks: RelativeTransfer<FileEntry>[], targetBasePath: string) {
  const groupedRoots = new Map<string, { total: number; sourcePath: string; hasFile: boolean }>()
  const byRoot = new Map<string, RelativeTransfer<FileEntry>[]>()
  for (const task of tasks) {
    const group = byRoot.get(task.rootName)
    if (group) group.push(task)
    else byRoot.set(task.rootName, [task])
  }
  for (const [rootName, group] of byRoot) {
    if (!group.some(task => task.isDir || task.relativePath.includes('/'))) continue
    const rootTask = group.find(task => task.relativePath === rootName) || group[0]
    groupedRoots.set(rootName, {
      total: group.reduce((sum, task) => sum + (task.isDir ? 0 : task.payload.size || 0), 0),
      sourcePath: rootTask?.payload.path || rootName,
      hasFile: group.some(task => !task.isDir)
    })
  }
  const groups = new Map<string, TransferItem>()
  for (const [rootName, info] of groupedRoots) {
    groups.set(rootName, createTransferItem({
      name: rootName,
      kind: 'folder',
      sourceName: transferEndpointName(source),
      targetName: transferEndpointName(target),
      sourcePath: info.sourcePath,
      targetPath: joinPath(targetBasePath, rootName, target.kind),
      total: info.total,
      isGroup: true,
      expanded: false
    }))
  }
  return groups
}

function finishEmptyTransferGroups(groups: Map<string, TransferItem>) {
  for (const group of groups.values()) {
    const children = transferItems.value.filter(item => item.parentID === group.id)
    if (children.length === 0 && group.status === 'running') {
      finishTransferItem(group, 'done')
    } else {
      syncTransferGroup(group.id)
    }
  }
}

function isTransferCancelledError(error: unknown) {
  return error instanceof Error && /(cancel|取消)/i.test(error.message)
}

async function expandTransferEntries(source: FileTab, entries: FileEntry[]) {
  const tasks: RelativeTransfer<FileEntry>[] = []
  for (const entry of entries) {
    await collectTransferEntry(source, entry, entry.name, tasks)
  }
  return tasks
}

async function collectTransferEntry(source: FileTab, entry: FileEntry, relativePath: string, tasks: RelativeTransfer<FileEntry>[]) {
  const cleanRelative = sanitizeTransferRelativePath(relativePath)
  if (!cleanRelative) return
  tasks.push({
    payload: entry,
    relativePath: cleanRelative,
    displayPath: cleanRelative,
    rootName: cleanRelative.split('/')[0] ?? entry.name,
    isDir: entry.is_dir
  })
  if (!entry.is_dir) return
  const listing = await listTabDirectory(source, entry.path)
  for (const child of listing.entries) {
    await collectTransferEntry(source, child, `${cleanRelative}/${child.name}`, tasks)
  }
}

function orderTransferTasks<T>(tasks: RelativeTransfer<T>[]) {
  return [...tasks].sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1
    return relativeDepth(a.relativePath) - relativeDepth(b.relativePath)
  })
}

async function transferFileTask(source: FileTab, target: FileTab, task: RelativeTransfer<FileEntry>, targetBasePath: string, transferID: string) {
  if (source.kind === 'local' && target.kind === 'local') {
    await postJSON('/api/local/copy', { source_path: task.payload.path, target_path: targetBasePath, relative_path: task.relativePath, transfer_id: transferID })
  } else if (source.kind === 'local' && target.kind === 'remote') {
    await postJSON('/api/sftp/upload-local', { session: target.session, path: targetBasePath, local_path: task.payload.path, relative_path: task.relativePath, transfer_id: transferID })
  } else if (source.kind === 'remote' && target.kind === 'local') {
    await postJSON('/api/sftp/download-local', { session: source.session, path: task.payload.path, local_path: targetBasePath, relative_path: task.relativePath, transfer_id: transferID })
  } else {
    await postJSON('/api/sftp/copy-remote', {
      source_session: source.session,
      source_path: task.payload.path,
      target_session: target.session,
      target_path: targetBasePath,
      relative_path: task.relativePath,
      transfer_id: transferID
    })
  }
}

async function prepareRelativeTransfers<T>(tasks: RelativeTransfer<T>[], target: FileTab, targetBasePath: string) {
  const topLevelItems = new Map((await listTabDirectory(target, targetBasePath)).entries.map(entry => [entry.name, entry] as const))
  const resolved: RelativeTransfer<T>[] = []
  let applyAllAction: UploadConflictAction | null = null
  const groups = new Map<string, RelativeTransfer<T>[]>()

  for (const task of tasks) {
    const rootName = task.relativePath.split('/')[0] || task.rootName
    const group = groups.get(rootName)
    if (group) group.push(task)
    else groups.set(rootName, [{ ...task, rootName }])
  }

  for (const [rootName, group] of groups) {
    const isFolderTransfer = group.some(task => task.isDir || task.relativePath.includes('/'))
    const existing = topLevelItems.get(rootName) ?? null
    let action: UploadConflictAction = 'overwrite'

    if (existing) {
      const resolution: UploadConflictResolution = applyAllAction
        ? { action: applyAllAction, applyAll: true }
        : await promptConflict(rootName)
      if (resolution.applyAll) applyAllAction = resolution.action
      action = resolution.action
    }

    if (action === 'cancel') return { tasks: [], cancelled: true }
    if (action === 'skip') continue

    const finalRootName = action === 'suffix' ? uniqueSuffixedName(rootName, topLevelItems) : rootName
    for (const task of group) {
      const suffix = task.relativePath === rootName ? '' : task.relativePath.slice(rootName.length)
      const relativePath = `${finalRootName}${suffix}`
      resolved.push({ ...task, relativePath, displayPath: relativePath, rootName: finalRootName })
    }
    topLevelItems.set(finalRootName, {
      name: finalRootName,
      path: joinPath(targetBasePath, finalRootName, target.kind),
      is_dir: isFolderTransfer,
      size: 0,
      mode: '',
      mod_time: ''
    })
  }

  return { tasks: resolved, cancelled: false }
}

function promptConflict(name: string) {
  return new Promise<UploadConflictResolution>(resolve => {
    conflictDialog.value = {
      visible: true,
      name,
      applyAll: false,
      resolve
    }
  })
}

function chooseConflict(action: UploadConflictAction) {
  const dialog = conflictDialog.value
  if (!dialog.resolve) return
  dialog.resolve({ action, applyAll: dialog.applyAll })
  conflictDialog.value = {
    visible: false,
    name: '',
    applyAll: false,
    resolve: null
  }
}

async function ensureTargetDirectory(target: FileTab, relativePath: string, targetBasePath: string) {
  const targetPath = joinPath(targetBasePath, relativePath, target.kind)
  const existing = await findEntryAtPath(target, targetPath)
  if (existing) {
    if (!existing.is_dir) throw new Error(`目标已存在同名文件：${targetPath}`)
    return
  }
  if (target.kind === 'local') {
    await postJSON('/api/local/mkdir', { path: targetPath })
  } else {
    await postJSON('/api/sftp/mkdir', { session: target.session, path: targetPath })
  }
}

async function findEntryAtPath(tab: FileTab, path: string) {
  const parent = parentPath(path, tab.kind)
  const name = baseName(path)
  const listing = await listTabDirectory(tab, parent)
  return listing.entries.find(entry => entry.name === name) || null
}

async function listTabDirectory(tab: FileTab, path: string) {
  return tab.kind === 'local'
    ? await api<{ path: string; entries: FileEntry[] }>(`/api/local/list?path=${encodeURIComponent(path)}`)
    : await api<{ path: string; entries: FileEntry[] }>(`/api/sftp/list?session=${encodeURIComponent(tab.session || '')}&path=${encodeURIComponent(path)}`)
}

function relativeDepth(relativePath: string) {
  return splitRelativePath(relativePath).length
}

function splitRelativePath(relativePath: string) {
  return relativePath.replace(/\\/g, '/').split('/').filter(Boolean)
}

function sanitizeTransferRelativePath(relativePath: string) {
  return relativePath.replace(/\\/g, '/').split('/').filter(part => part && part !== '.' && part !== '..').join('/')
}

function normalizeTransferTasks<T>(tasks: RelativeTransfer<T>[]) {
  let invalidCount = 0
  let renamedCount = 0
  const normalized: RelativeTransfer<T>[] = []
  for (const task of tasks) {
    const originalPath = task.relativePath.replace(/\\/g, '/').split('/').filter(part => part && part !== '.' && part !== '..').join('/')
    const originalDisplayPath = (task.displayPath || task.relativePath).replace(/\\/g, '/').split('/').filter(part => part && part !== '.' && part !== '..').join('/')
    const relativePath = normalizeUploadRelativePath(originalPath)
    const displayPath = normalizeUploadRelativePath(originalDisplayPath)
    if (!relativePath || !isSafeUploadRelativePath(relativePath)) {
      invalidCount += 1
      continue
    }
    if (relativePath !== originalPath || displayPath !== originalDisplayPath) renamedCount += 1
    normalized.push({
      ...task,
      relativePath,
      displayPath,
      rootName: relativePath.split('/')[0] ?? task.rootName
    })
  }
  return { tasks: normalized, invalidCount, renamedCount }
}

function uniqueSuffixedName(name: string, existing: Map<string, FileEntry>) {
  let candidate = `${name}.new`
  while (existing.has(candidate)) candidate = `${candidate}.new`
  return candidate
}

function canDownload(paneName: PaneName) {
  const tab = activeTab(paneName)
  return Boolean(tab?.kind === 'remote' && tab.selected.length === 1 && !tab.selected[0].is_dir)
}

function renameSelected(paneName: PaneName) {
  const entry = selectedEntries(paneName)[0]
  if (!entry) return
  return renameEntry(paneName, entry)
}

async function openFile(paneName: PaneName, entry: FileEntry) {
  const tab = activeTab(paneName)
  if (!tab || entry.is_dir) return
  if (tab.kind === 'local') {
    await postJSON('/api/local/open', { path: entry.path })
    log(`打开本地文件：${entry.path}`)
    return
  }
  if (!tab.session) return
  const result = await postJSON<{ path: string }>('/api/sftp/open', { session: tab.session, path: entry.path })
  log(`打开远程文件缓存：${entry.path} -> ${result.path}`)
}

async function openTextFile(paneName: PaneName, entry: FileEntry) {
  const tab = activeTab(paneName)
  if (!tab || entry.is_dir) return
  if (tab.kind === 'local') {
    await postJSON('/api/local/open-text', { path: entry.path })
    log(`用记事本编辑本地文件：${entry.path}`)
    return
  }
  if (!tab.session) return
  const result = await postJSON<{ path: string }>('/api/sftp/open-text', { session: tab.session, path: entry.path })
  log(`用记事本编辑远程文件缓存：${entry.path} -> ${result.path}`)
}

async function copyEntryPath(entry: FileEntry) {
  await writeClipboard(entry.path)
  ElMessage.success('已复制路径')
}

async function writeClipboard(value: string) {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(value)
      return
    }
  } catch {
    // WebView2 may deny the async Clipboard API; fall back to the legacy copy command.
  }
  const input = document.createElement('textarea')
  input.value = value
  input.setAttribute('readonly', 'true')
  input.style.position = 'fixed'
  input.style.left = '-9999px'
  input.style.top = '0'
  document.body.appendChild(input)
  input.select()
  const copied = document.execCommand('copy')
  input.remove()
  if (!copied) throw new Error('复制路径失败')
}

async function renameEntry(paneName: PaneName, entry: FileEntry) {
  const tab = activeTab(paneName)
  if (!tab) return
  let result: { value: string }
  try {
    result = await ElMessageBox.prompt('输入新的名称', `重命名：${entry.name}`, {
      inputValue: entry.name,
      inputPattern: safeNamePattern,
      inputErrorMessage: '名称不能包含路径分隔符',
      confirmButtonText: '确定',
      cancelButtonText: '取消'
    })
  } catch {
    return
  }
  const newName = result.value.trim()
  if (!newName || newName === entry.name) return
  const newPath = joinPath(parentPath(entry.path, tab.kind), newName, tab.kind)
  if (tab.kind === 'local') {
    await postJSON('/api/local/rename', { path: entry.path, new_path: newPath })
  } else {
    await postJSON('/api/sftp/rename', { session: tab.session, path: entry.path, new_path: newPath })
  }
  log(`重命名：${entry.path} -> ${newPath}`)
  await loadTab(paneName, tab)
}

function downloadSelected(paneName: PaneName) {
  const tab = activeTab(paneName)
  const entry = tab?.selected[0]
  if (!tab?.session || !entry || entry.is_dir) return
  const url = `/api/sftp/download?session=${encodeURIComponent(tab.session)}&path=${encodeURIComponent(entry.path)}`
  window.open(url, '_blank', 'noopener')
}

function pickUpload(paneName: PaneName, kind: UploadKind) {
  uploadInputs[paneName][kind]?.click()
}

async function handleUploadInput(paneName: PaneName, event: Event) {
  const input = event.target as HTMLInputElement
  const files = Array.from(input.files || [])
  input.value = ''
  await uploadEntries(paneName, filesToUploadEntries(files))
}

function hasExternalFiles(event: DragEvent) {
  return Array.from(event.dataTransfer?.types || []).includes('Files')
}

function hasAppDrag(event: DragEvent) {
  return Array.from(event.dataTransfer?.types || []).includes(appDragType)
}

function isFolderDropTarget(paneName: PaneName, entry: FileEntry) {
  return folderDropTarget.value?.pane === paneName && folderDropTarget.value.path === entry.path
}

function isCurrentDirectoryDropActive(paneName: PaneName) {
  return panes[paneName].dropActive && Boolean(dragging.value && dragging.value.pane !== paneName)
}

function startFileDrag(paneName: PaneName, index: number, entry: FileEntry, event: DragEvent) {
  const tab = activeTab(paneName)
  if (!tab || !event.dataTransfer || !exportDragArmed.value || !exportDragPathSet.value.has(entry.path)) {
    event.preventDefault()
    finishFileDrag()
    return
  }
  const items = dragItemsForIndex(paneName, index)
  if (items.length === 0) {
    event.preventDefault()
    finishFileDrag()
    return
  }
  exportDragActive.value = true
  resetPressState()
  const paths = items.map(item => item.path)
  const payload = { pane: paneName, tabID: tab.id, paths }
  dragging.value = payload
  event.dataTransfer.effectAllowed = 'copyMove'
  event.dataTransfer.setData(appDragType, JSON.stringify(payload))
  event.dataTransfer.setData('text/plain', paths.join('\n'))
  setDragImage(event, items)
}

function finishFileDrag() {
  cancelLongPress()
  dragging.value = null
  folderDropTarget.value = null
  exportDragActive.value = false
  exportDragArmed.value = false
  exportDragPathSet.value = new Set()
  paneNames.forEach(paneName => {
    panes[paneName].dropActive = false
  })
}

function setDragImage(event: DragEvent, items: FileEntry[]) {
  if (!event.dataTransfer) return
  const image = document.createElement('div')
  image.className = 'file-drag-image'
  image.textContent = items.length === 1 ? items[0].name : `${items.length} files`
  document.body.appendChild(image)
  event.dataTransfer.setDragImage(image, 12, 12)
  window.setTimeout(() => image.remove(), 0)
}

function folderDragPayload(event: DragEvent) {
  const payloadText = event.dataTransfer?.getData(appDragType)
  if (payloadText) {
    try {
      return JSON.parse(payloadText) as DragPayload
    } catch {
      return null
    }
  }
  return dragging.value
}

function handleFolderDragOver(paneName: PaneName, entry: FileEntry, event: DragEvent) {
  if (!entry.is_dir || (!hasExternalFiles(event) && !hasAppDrag(event))) return
  const valid = hasExternalFiles(event)
    ? Boolean(activeTab(paneName)?.kind === 'remote')
    : isValidFolderTransferTarget(paneName, entry, folderDragPayload(event))
  if (!valid) {
    if (event.dataTransfer) event.dataTransfer.dropEffect = 'none'
    if (isFolderDropTarget(paneName, entry)) folderDropTarget.value = null
    return
  }

  event.preventDefault()
  event.stopPropagation()
  folderDropTarget.value = { pane: paneName, path: entry.path }
  if (event.dataTransfer) event.dataTransfer.dropEffect = hasExternalFiles(event) ? 'copy' : 'move'
}

function handleFolderDragLeave(paneName: PaneName, entry: FileEntry, event: DragEvent) {
  if (!isFolderDropTarget(paneName, entry)) return
  const row = event.currentTarget instanceof HTMLElement ? event.currentTarget : null
  const related = event.relatedTarget instanceof Node ? event.relatedTarget : null
  if (row && related && row.contains(related)) return
  folderDropTarget.value = null
}

async function handleFolderDrop(paneName: PaneName, entry: FileEntry, event: DragEvent) {
  if (!entry.is_dir || (!hasExternalFiles(event) && !hasAppDrag(event))) return
  const payload = folderDragPayload(event)
  const external = hasExternalFiles(event)
  if (!external && !isValidFolderTransferTarget(paneName, entry, payload)) return
  if (external && activeTab(paneName)?.kind !== 'remote') return

  event.preventDefault()
  event.stopPropagation()
  panes[paneName].dropActive = false
  folderDropTarget.value = null

  if (external) {
    await uploadEntries(paneName, await collectDropUploadEntries(event), entry.path)
    return
  }
  if (payload) await dropInternalFiles(paneName, JSON.stringify(payload), entry.path)
}

function isValidFolderTransferTarget(targetPane: PaneName, target: FileEntry, payload: DragPayload | null) {
  if (!target.is_dir || !payload || payload.paths.length === 0) return false
  const source = tabByID(payload.pane, payload.tabID)
  if (!source) return false
  const targetPath = normalizeComparablePath(target.path)
  return source.entries
    .filter(entry => payload.paths.includes(entry.path))
    .every(entry => {
      const sourcePath = normalizeComparablePath(entry.path)
      if (sourcePath === targetPath) return false
      if (entry.is_dir && isPathInside(targetPath, sourcePath)) return false
      return true
    })
}

function normalizeComparablePath(path: string) {
  return path.replace(/\\/g, '/').replace(/\/+$/, '')
}

function isPathInside(path: string, parent: string) {
  return path === parent || path.startsWith(`${parent}/`)
}

function isValidCurrentDirectoryTransfer(targetPane: PaneName, payload: DragPayload | null) {
  if (!payload || payload.pane === targetPane || payload.paths.length === 0) return false
  const source = activeTab(payload.pane)
  const target = activeTab(targetPane)
  return Boolean(source && target && source.id === payload.tabID)
}

function handleCurrentDirectoryDropOver(paneName: PaneName, event: DragEvent) {
  if (!hasAppDrag(event)) return
  const payload = folderDragPayload(event)
  if (!isValidCurrentDirectoryTransfer(paneName, payload)) {
    if (event.dataTransfer) event.dataTransfer.dropEffect = 'none'
    return
  }
  event.preventDefault()
  panes[paneName].dropActive = true
  if (event.dataTransfer) event.dataTransfer.dropEffect = 'copy'
}

function handleCurrentDirectoryDropLeave(paneName: PaneName, event: DragEvent) {
  const target = event.currentTarget instanceof HTMLElement ? event.currentTarget : null
  const related = event.relatedTarget instanceof Node ? event.relatedTarget : null
  if (target && related && target.contains(related)) return
  if (dragging.value?.pane !== paneName) panes[paneName].dropActive = false
}

async function handleCurrentDirectoryDrop(paneName: PaneName, event: DragEvent) {
  if (!hasAppDrag(event)) return
  const payload = folderDragPayload(event)
  panes[paneName].dropActive = false
  folderDropTarget.value = null
  if (!isValidCurrentDirectoryTransfer(paneName, payload)) return
  event.preventDefault()
  if (payload) await dropInternalFiles(paneName, JSON.stringify(payload))
}

function handleDragEnter(paneName: PaneName, event: DragEvent) {
  if (!hasExternalFiles(event) && !hasAppDrag(event)) return
  event.preventDefault()
  if (dragging.value?.pane === paneName) return
  panes[paneName].dropActive = true
}

function handleDragOver(paneName: PaneName, event: DragEvent) {
  if (!hasExternalFiles(event) && !hasAppDrag(event)) return
  event.preventDefault()
  setUploadDropEffect(event)
  if (dragging.value?.pane === paneName) return
  panes[paneName].dropActive = true
}

function handleDragLeave(paneName: PaneName, event: DragEvent) {
  const target = event.currentTarget as HTMLElement
  if (event.relatedTarget instanceof Node && target.contains(event.relatedTarget)) return
  panes[paneName].dropActive = false
}

async function handleDrop(paneName: PaneName, event: DragEvent) {
  if (!hasExternalFiles(event) && !hasAppDrag(event)) return
  event.preventDefault()
  panes[paneName].dropActive = false
  const payloadText = event.dataTransfer?.getData(appDragType)
  if (payloadText) {
    try {
      await dropInternalFiles(paneName, payloadText)
    } finally {
      finishFileDrag()
    }
    return
  }
  await uploadEntries(paneName, await collectDropUploadEntries(event))
}

async function dropInternalFiles(targetPane: PaneName, payloadText: string, targetPathOverride?: string) {
  const payload = JSON.parse(payloadText) as DragPayload
  if (payload.pane === targetPane && !targetPathOverride) return
  const source = activeTab(payload.pane)
  if (!source || source.id !== payload.tabID) return
  const entriesByPath = new Map(source.entries.map(entry => [entry.path, entry]))
  const entries = payload.paths.map(path => entriesByPath.get(path)).filter((entry): entry is FileEntry => Boolean(entry))
  await transferEntries(payload.pane, targetPane, entries, targetPathOverride)
}

async function uploadEntries(paneName: PaneName, entries: UploadEntry[], targetPathOverride?: string) {
  const tab = activeTab(paneName)
  if (!tab || tab.kind !== 'remote' || !tab.session) {
    ElMessage.warning('请先在目标窗口打开远程 SFTP 标签页')
    return
  }
  const targetBasePath = targetPathOverride || tab.path
  const normalized = normalizeUploadEntries(entries)
  if (normalized.invalidCount > 0) ElMessage.error(`已跳过 ${normalized.invalidCount} 个不安全路径`)
  if (normalized.renamedCount > 0) ElMessage.info(`已规范化 ${normalized.renamedCount} 个上传路径`)
  const tasks = normalized.entries.map(entry => ({
    payload: entry,
    relativePath: entry.relativePath,
    displayPath: entry.displayPath,
    rootName: entry.rootName,
    isDir: false
  }) satisfies RelativeTransfer<UploadEntry>)
  const prepared = await prepareRelativeTransfers(tasks, tab, targetBasePath)
  if (prepared.cancelled || prepared.tasks.length === 0) return
  const folderGroups = new Map<string, TransferItem>()
  const folderTotals = new Map<string, number>()
  for (const task of prepared.tasks) {
    if (!task.relativePath.includes('/')) continue
    folderTotals.set(task.rootName, (folderTotals.get(task.rootName) || 0) + (task.payload.file.size || 0))
  }
  for (const task of prepared.tasks) {
    let parentID: string | undefined
    if (task.relativePath.includes('/')) {
      let group = folderGroups.get(task.rootName)
      if (!group) {
        group = createTransferItem({
          name: task.rootName,
          kind: 'folder',
          sourceName: t('sftp.local'),
          targetName: transferEndpointName(tab),
          sourcePath: task.rootName,
          targetPath: joinPath(targetBasePath, task.rootName, tab.kind),
          total: folderTotals.get(task.rootName) || 0,
          isGroup: true,
          expanded: false
        })
        folderGroups.set(task.rootName, group)
      }
      parentID = group.id
    }
    const form = new FormData()
    form.append('session', tab.session)
    form.append('path', targetBasePath)
    form.append('relative_path', task.relativePath)
    form.append('file', task.payload.file)
    const item = createTransferItem({
      name: task.displayPath,
      kind: 'file',
      sourceName: t('sftp.local'),
      targetName: transferEndpointName(tab),
      sourcePath: task.payload.displayPath,
      targetPath: joinPath(targetBasePath, task.relativePath, tab.kind),
      total: task.payload.file.size || 0,
      parentID
    })
    form.append('transfer_id', item.id)
    if (parentID) syncTransferGroup(parentID)
    log(`上传：${task.displayPath} -> ${targetBasePath}`)
    try {
      await runWithTransferProgress(item, item.id, () => uploadForm(form))
      finishTransferItem(item, 'done')
    } catch (error) {
      finishTransferItem(item, 'error', (error as Error).message)
      throw error
    }
  }
  await loadTab(paneName, tab)
}

async function runWithTransferProgress(item: TransferItem, transferID: string, action: () => Promise<void>) {
  let stopped = false
  let timer: number | undefined
  let progressError: Error | null = null
  const poll = async () => {
    if (stopped) return
    let progress: TransferProgress
    try {
      progress = await api<TransferProgress>(`/api/transfer-progress?id=${encodeURIComponent(transferID)}`)
    } catch {
      // The transfer may not have reached the backend copy stage yet.
      return
    }
    const target = currentTransferItem(item)
    target.paused = progress.paused
    target.cancelled = progress.cancelled
    updateTransferItem(target, progress.loaded, progress.total || target.total)
    if (progress.cancelled) {
      stopped = true
      throw new Error(progress.error || t('sftp.transferCancelled'))
    }
    if (progress.done && progress.error) {
      stopped = true
      throw new Error(progress.error)
    }
    if (progress.done) stopped = true
  }
  timer = window.setInterval(() => {
    poll().catch(error => {
      progressError = error as Error
      stopped = true
    })
  }, TRANSFER_PROGRESS_POLL_MS)
  try {
    await poll()
    await action()
    if (progressError) throw progressError
    await poll()
    if (progressError) throw progressError
    const target = currentTransferItem(item)
    if (target.cancelled) throw new Error(target.error || t('sftp.transferCancelled'))
    if (target.status === 'running') updateTransferItem(target, target.total, target.total)
  } finally {
    stopped = true
    if (timer !== undefined) window.clearInterval(timer)
  }
}

function uploadForm(form: FormData) {
  return new Promise<void>((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', '/api/sftp/upload')
    const token = launcherToken()
    if (token) xhr.setRequestHeader(launcherTokenHeader, token)
    xhr.onload = () => {
      const text = xhr.responseText || '{}'
      let data: any = {}
      try {
        data = JSON.parse(text)
      } catch {
        data = {}
      }
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve()
      } else {
        reject(new Error(data.error || t('sftp.uploadFailed')))
      }
    }
    xhr.onerror = () => reject(new Error(t('sftp.uploadFailed')))
    xhr.send(form)
  })
}

function dropHint(paneName: PaneName) {
  if (dragging.value) return '松开后传输到此窗口'
  return activeTab(paneName)?.kind === 'remote' ? '拖拽文件上传到当前远程目录' : '本地窗口只能接收另一个窗口的文件'
}

async function refreshAll() {
  await Promise.all(paneNames.map(paneName => loadTab(paneName)))
}

async function pollOpenSyncEvents() {
  if (openSyncPolling) return
  openSyncPolling = true
  try {
    const result = await api<{ events: OpenSyncEvent[] }>(`/api/sftp/open-sync-events?after=${lastOpenSyncSeq}`)
    const events = Array.isArray(result.events) ? result.events : []
    const refreshKeys = new Set<string>()
    for (const event of events) {
      lastOpenSyncSeq = Math.max(lastOpenSyncSeq, event.seq || 0)
      if (event.status === 'done') {
        log(t('sftp.openSyncSuccess', { path: event.remote_path }))
        collectRemoteRefreshKeys(event.session, refreshKeys)
      } else {
        log(t('sftp.openSyncFailed', { path: event.remote_path, error: event.error || t('sftp.unknown') }))
      }
    }
    if (refreshKeys.size > 0) await refreshRemoteTabs(refreshKeys)
  } finally {
    openSyncPolling = false
  }
}

function collectRemoteRefreshKeys(session: string, refreshKeys: Set<string>) {
  if (!session) return
  paneNames.forEach(paneName => {
    panes[paneName].tabs.forEach(tab => {
      if (tab.kind === 'remote' && tab.session === session) {
        refreshKeys.add(`${paneName}:${tab.id}`)
      }
    })
  })
}

async function refreshRemoteTabs(refreshKeys: Set<string>) {
  const tasks: Promise<void>[] = []
  refreshKeys.forEach(key => {
    const [paneName, tabID] = key.split(':') as [PaneName, string]
    const tab = panes[paneName]?.tabs.find(item => item.id === tabID)
    if (tab?.kind === 'remote') tasks.push(loadTab(paneName, tab))
  })
  await Promise.all(tasks)
}

function handleContextMenuDocumentClick(event: MouseEvent) {
  const target = event.target
  if (target instanceof HTMLElement && target.closest('.sftp-context-menu')) return
  closeContextMenu()
}

function handleContextMenuKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape') closeContextMenu()
}

onMounted(async () => {
  if (typeof ResizeObserver !== 'undefined') {
    scrollResizeObserver = new ResizeObserver(() => {
      paneNames.forEach(paneName => updateVirtualMetrics(paneName))
      updateTransferScrollbars()
    })
    paneNames.forEach(paneName => {
      observeScrollElement(panes[paneName].treeRef)
      observeScrollElement(panes[paneName].bodyRef)
    })
    observeScrollElement(transferListRef.value)
  }
  window.addEventListener('mouseup', finishSelection)
  document.addEventListener('click', handleContextMenuDocumentClick)
  window.addEventListener('keydown', handleContextMenuKeydown)
  window.addEventListener('scroll', closeContextMenu, true)
  window.addEventListener('beforeunload', handleBeforeUnload)
  openSyncPollTimer = window.setInterval(() => {
    pollOpenSyncEvents().catch(error => {
      log(t('sftp.openSyncFailed', { path: t('sftp.unknown'), error: (error as Error).message }))
    })
  }, 1500)
  profiles.value = await api<Profile[]>('/api/profiles')
  const homeData = await api<{ path: string }>('/api/local/home')
  home.value = homeData.path || '.'
  paneNames.forEach(paneName => {
    panes[paneName].draft.localPath = home.value
    panes[paneName].draft.profileID = remoteProfiles.value[0]?.id || ''
    const tab = localTab(home.value)
    panes[paneName].tabs.push(tab)
    panes[paneName].active = tab.id
  })
  await refreshAll()
  await nextTick()
  paneNames.forEach(paneName => updateVirtualMetrics(paneName))
  updateTransferScrollbars()
})

onBeforeUnmount(() => {
  if (conflictDialog.value.resolve) chooseConflict('cancel')
  stopColumnResize()
  stopScrollbarThumbDrag()
  stopTransferScrollbarThumbDrag()
  scrollResizeObserver?.disconnect()
  scrollResizeObserver = null
  window.removeEventListener('mouseup', finishSelection)
  document.removeEventListener('click', handleContextMenuDocumentClick)
  window.removeEventListener('keydown', handleContextMenuKeydown)
  window.removeEventListener('scroll', closeContextMenu, true)
  window.removeEventListener('beforeunload', handleBeforeUnload)
  if (openSyncPollTimer !== undefined) {
    window.clearInterval(openSyncPollTimer)
    openSyncPollTimer = undefined
  }
  window.removeEventListener('mousemove', handleBlankDragMove)
  window.removeEventListener('mousemove', handlePressMove)
  cancelLongPress()
  paneNames.forEach(paneName => {
    panes[paneName].tabs.forEach(tab => {
      if (tab.kind === 'remote' && tab.session) {
        postJSON('/api/sftp/disconnect', { session: tab.session }).catch(() => undefined)
      }
    })
  })
})
</script>
