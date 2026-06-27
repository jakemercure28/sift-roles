const js = require('@eslint/js');
const globals = require('globals');
const prettier = require('eslint-config-prettier');

module.exports = [
  // Ignore generated artifacts, deps, and gitignored personal data.
  {
    ignores: [
      'node_modules/',
      '.claude/worktrees/',
      'scraper-service/',
      'public/bookmarklet.js',
      'coverage/',
      '*.db',
      '*.pdf',
      'jobs.json',
      'data/',
      '.context/',
    ],
  },

  // Base config: Node.js (CommonJS) for all JS files.
  {
    files: ['**/*.js'],
    languageOptions: {
      sourceType: 'commonjs',
      ecmaVersion: 2024,
      globals: { ...globals.node },
    },
    rules: {
      ...js.configs.recommended.rules,
      'no-unused-vars': [
        'warn',
        {
          argsIgnorePattern: '^_',
          varsIgnorePattern: '^_',
          caughtErrorsIgnorePattern: '^_',
        },
      ],
      // Intentional error-swallowing catch blocks are common here; still flag
      // genuinely empty if/for/while blocks.
      'no-empty': ['error', { allowEmptyCatch: true }],
      'prefer-const': 'warn',
      'no-var': 'error',
      eqeqeq: ['warn', 'smart'],
    },
  },

  // Browser code: plain <script> files loaded together on the dashboard page,
  // so functions defined in one file are shared globals in the others.
  {
    files: ['public/js/**/*.js'],
    languageOptions: {
      sourceType: 'script',
      globals: {
        ...globals.browser,
        navigateDashboardUrl: 'readonly',
        performUndo: 'readonly',
        registerUndo: 'readonly',
        SCRAMBLE_PHRASES: 'readonly',
        showToast: 'readonly',
        startScrambleLoader: 'readonly',
        syncThemeIcon: 'readonly',
      },
    },
    rules: {
      // These shared globals are intentionally defined in one script and used
      // in its siblings; the definition site is not a redeclaration.
      'no-redeclare': ['error', { builtinGlobals: false }],
      // Top-level functions here are wired to inline HTML on* handlers, so they
      // look unused to static analysis. Only flag unused *local* variables.
      'no-unused-vars': [
        'warn',
        {
          vars: 'local',
          argsIgnorePattern: '^_',
          caughtErrorsIgnorePattern: '^_',
        },
      ],
    },
  },

  // Disable stylistic rules that conflict with Prettier (must be last).
  prettier,
];
