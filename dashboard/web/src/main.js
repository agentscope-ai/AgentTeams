import { renderOverview } from './panels/overview.js';
import { renderTasks } from './panels/tasks.js';
import { renderBoard } from './panels/board.js';
import { renderProjects } from './panels/projects.js';
import { renderFiles } from './panels/files.js';
import { renderHealth } from './panels/health.js';

const panels = {
  overview: renderOverview,
  tasks: renderTasks,
  board: renderBoard,
  projects: renderProjects,
  files: renderFiles,
  health: renderHealth,
};

const main = document.getElementById('main');
const tabButtons = document.querySelectorAll('.tab-btn');

let currentCleanup = null;

function activateTab(name) {
  if (currentCleanup) {
    currentCleanup();
    currentCleanup = null;
  }
  tabButtons.forEach((btn) => btn.classList.toggle('active', btn.dataset.tab === name));
  main.innerHTML = '';
  currentCleanup = panels[name](main);
}

tabButtons.forEach((btn) => {
  btn.addEventListener('click', () => activateTab(btn.dataset.tab));
});

activateTab('overview');
