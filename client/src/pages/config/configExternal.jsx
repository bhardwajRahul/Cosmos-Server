import * as React from 'react';
import MainCard from '../../components/MainCard';
import {
  Grid,
  Stack,
  FormHelperText,
} from '@mui/material';
import { CosmosCheckbox, CosmosCollapse, CosmosInputPassword, CosmosInputText } from './users/formShortcuts';
import { useTranslation } from 'react-i18next';
import { FilePickerButton } from '../../components/filePicker';

const ConfigExternal = ({ formik }) => {
  const { t } = useTranslation();

  return (
    <Stack spacing={3}>
      <MainCard title="Docker">
        <Stack spacing={2}>
          <CosmosCheckbox
            label={t('mgmt.config.docker.skipPruneNetworkCheckbox.skipPruneNetworkLabel')}
            name="SkipPruneNetwork"
            formik={formik}
          />

          <CosmosCheckbox
            label={t('mgmt.config.docker.skipPruneImageCheckbox.skipPruneImageLabel')}
            name="SkipPruneImages"
            formik={formik}
          />

          <Stack direction={"row"} spacing={2} alignItems="flex-end">
            <FilePickerButton onPick={(path) => {
              if(path)
                formik.setFieldValue('DefaultDataPath', path);
            }} size="150%" select="folder" />
            <CosmosInputText
              label={t('mgmt.config.docker.defaultDatapathInput.defaultDatapathLabel')}
              name="DefaultDataPath"
              formik={formik}
              placeholder={'/cosmos-storage'}
            />
          </Stack>
        </Stack>
      </MainCard>

      <MainCard title={t('mgmt.config.general.monitoringDbTitle')}>
        <Grid container spacing={3}>
          <Grid item xs={12}>
            <Stack spacing={1}>
              <FormHelperText>{t('mgmt.config.general.monitoringDb.introHelperText')}</FormHelperText>

              <CosmosCollapse title={t('mgmt.config.general.monitoringDb.postgresTitle')}>
                <Grid container spacing={3}>
                  <Grid item xs={12}>
                    <CosmosInputText
                      label={t('mgmt.config.general.monitoringDb.hostInput.hostLabel')}
                      name="PostgresHost"
                      formik={formik}
                      placeholder={'localhost:5432'}
                    />
                    <FormHelperText>{t('mgmt.config.general.monitoringDb.hostInput.hostHelperText')}</FormHelperText>
                  </Grid>

                  <Grid item xs={12}>
                    <CosmosInputText
                      label={t('mgmt.config.general.monitoringDb.databaseInput.databaseLabel')}
                      name="PostgresDatabase"
                      formik={formik}
                      placeholder={'cosmos'}
                    />

                    <CosmosInputText
                      label={t('mgmt.config.general.monitoringDb.usernameInput.usernameLabel')}
                      name="PostgresUsername"
                      formik={formik}
                    />

                    <CosmosInputPassword
                      label={t('mgmt.config.general.monitoringDb.passwordInput.passwordLabel')}
                      name="PostgresPassword"
                      autoComplete='new-password'
                      formik={formik}
                      noStrength
                    />
                  </Grid>

                  <Grid item xs={12}>
                    <CosmosInputText
                      label={t('mgmt.config.general.monitoringDb.nodeNameInput.nodeNameLabel')}
                      name="MetricsNodeName"
                      formik={formik}
                    />
                    <FormHelperText>{t('mgmt.config.general.monitoringDb.nodeNameInput.nodeNameHelperText')}</FormHelperText>
                  </Grid>
                </Grid>
              </CosmosCollapse>
            </Stack>
          </Grid>
        </Grid>
      </MainCard>
    </Stack>
  );
};

export default ConfigExternal;
