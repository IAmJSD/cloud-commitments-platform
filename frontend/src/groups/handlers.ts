/**
 * Event handlers setup for group management
 */

import { openCreateGroupModal, closeGroupModal, saveGroup, addPermission } from './groupModals';
import { wireGroupForm } from './groupFormValidation';

/**
 * Setup event handlers for group management
 */
export function setupGroupHandlers(): void {
  // Make functions globally available for modal buttons that still need onclick handlers
  (window as any).openCreateGroupModal = openCreateGroupModal;
  (window as any).closeGroupModal = closeGroupModal;
  (window as any).addPermission = () => addPermission();

  // Setup form handlers
  // Idempotent: index.ts wires the same form at startup (issue #213).
  wireGroupForm((e) => void saveGroup(e));
}
